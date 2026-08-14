# RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test

| Field | Value |
| --- | --- |
| Public id | `10491a01-30ae-4699-b5f1-a1993e026dd8` |
| Key | RELAY-25 |
| Epic | [RELAY](../epic.md) |
| Status | in_progress |
| Priority | P0 |
| Component | relay |
| Section | backlog |
| Tags | vacuous-today, epic-deliverable |
| Created | 2026-08-08T15:56:48.369932+00:00 |
| Updated | 2026-08-14T20:50:20.490508+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/fed-smoke.sh
```

## Status note

Owner codex-1; RELAY-25 remains in progress and must not be completed until the non-vacuous three-bus live proof passes. Current atomic follow-up acceptance: (1) fail-closed preflight names occupied 9101/9102/9103 ports and stale /tmp/fed-smoke-{a,b,c} roots before mutation, prints exact manual remediation, and never deletes them; (2) a zero-delivery C watch reads C audit first and, when the target record has bus_path [A,B,C], reports watch timeout/environmental slowness rather than relay failure, otherwise preserves relay/audit failure attribution; (3) read_audit uses each stopped per-bus server binary via agent-bus log --data-dir, never agent-busctl; (4) syntax/static/focused diagnostics, reviewer, and security gates pass. Full bash scripts/fed-smoke.sh may remain dependency-RED pending RELAY-24 and the recorded federation dependency closure; that RED is not a completion claim.

## Description

FEDERATION phase, wave 5. Deps: RELAY-24 (composition root).

scripts/fed-smoke.sh: three buses on 127.0.0.1:9101/9102/9103, data dirs
/tmp/fed-smoke-{a,b,c} (NEVER the tracked data/ dir). Peers A<->B and B<->C via `agent-bus peer
add`, with A carrying `--route-for busC`. An agent on A sends to an agent on C; C receives it
EXACTLY ONCE; the audit log on each hop shows the bus path, and C's audit entry shows all three
hops (A, B, C).

The script header MUST state what loopback does NOT prove: tunnel bring-up/flap, NAT/keepalive,
latency vs RetryHorizonCeiling, pinning through a tunnel. A follow-up task covers a real
three-host run over actual SSH tunnels; the loopback smoke does not substitute for it.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [CLI-11](../../UNASSIGNED/CLI-11--bf966c07/task.md)
- **blocked by** [CLI-6](../../CLI/CLI-6--47001cb4/task.md)
- **blocked by** [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md)
- **blocked by** [INVITE-CLIENT](../../INVITE/INVITE-CLIENT--4123e25d/task.md)
- **blocked by** [RELAY-24](../RELAY-24--e303c624/task.md)
- **blocked by** [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md)
- **blocked by** [RELAY-24-BLOCKER-PEERCERTFLAG](../RELAY-24-BLOCKER-PEERCERTFLAG--0e6b5a49/task.md)
- **blocked by** [SIGN-1-FU-OUTOFORDER-POISON](../../SIGN/SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md)
- **blocked by** [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md)
- **relates to** [RELAY-25-FU-REALHOST](../RELAY-25-FU-REALHOST--8708f7c9/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-10](../../AGENTIF/AGENTIF-10--1e837ac9/task.md) — AGENTIF-10: bus-serve pidfile process identity and PID-reuse-safe final stop (todo)
- [CLI-11](../../UNASSIGNED/CLI-11--bf966c07/task.md) — CLI-11: export the bus signing public key from the operator CLI (done)
- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (done)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [INVITE-CLIENT](../../INVITE/INVITE-CLIENT--4123e25d/task.md) — INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -… (done)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md) — RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.Ne… (todo)
- [RELAY-24-BLOCKER-HUBINGEST](../RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) — RELAY-24-BLOCKER-HUBINGEST: internal/hub exported relay-ingest entry point -- foreign sen… (done)
- [RELAY-24-BLOCKER-PEERCERTFLAG](../RELAY-24-BLOCKER-PEERCERTFLAG--0e6b5a49/task.md) — RELAY-24-BLOCKER-PEERCERTFLAG: agent-bus peer add has no flag to bind a peer's inbound cl… (todo)
- [RELAY-25-FU-REALHOST](../RELAY-25-FU-REALHOST--8708f7c9/task.md) — RELAY-25-FU-REALHOST: Real three-host SSH-tunnel federation run -- loopback smoke does no… (todo)
- [RELAY-26](../RELAY-26--d72a1e04/task.md) — RELAY-26: Startup refusal: non-loopback -listen with peer records and invite-gating off (todo)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [SIGN-1-FU-OUTOFORDER-POISON](../../SIGN/SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) — SIGN-1-FU-OUTOFORDER-POISON: Reserve-then-send lets mints be spent out of order, which pe… (done)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (superseded)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
