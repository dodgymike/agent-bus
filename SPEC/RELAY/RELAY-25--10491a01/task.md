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
| Updated | 2026-08-14T11:32:43.079224+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/fed-smoke.sh
```

## Status note

Assigned to codex-1 for RELAY-25 implementation. Scope: scripts/fed-smoke.sh plus narrowly necessary test/docs wiring; depends on RELAY-24. Commit dc29d46c625ded0e9b004992a0c22c4e76e5fb4f landed scripts/fed-smoke.sh alone. Evidence: full `bash scripts/fed-smoke.sh` proof is RED/non-passing because required compiled CLI/runtime federation capabilities are not yet available; this is not a completion claim. Remaining blockers: CLI-11 (bf966c07-5f99-4fe6-bb23-52868ed04c33) public-key export, CLI-6 (47001cb4-bc0f-44f8-929e-ac51bc6d0fb3) audit bus_path output, invite/enrolment tasks, RELAY-41, RELAY-45, RELAY-20, RELAY-21, RELAY-24, SIGN-6, and CRYPTO-10. Must prove non-vacuous three-bus A->B->C exactly-once delivery, audit bus path A/B/C, and document loopback limitations. Do not touch tracked data/ or unrelated tasks/files.

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
- **blocked by** [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md)
- **relates to** [RELAY-25-FU-REALHOST](../RELAY-25-FU-REALHOST--8708f7c9/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-11](../../UNASSIGNED/CLI-11--bf966c07/task.md) — CLI-11: export the bus signing public key from the operator CLI (todo)
- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (in_progress)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (todo)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (todo)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (in_progress)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-10](../../AGENTIF/AGENTIF-10--1e837ac9/task.md) — AGENTIF-10: bus-serve pidfile process identity and PID-reuse-safe final stop (todo)
- [CLI-11](../../UNASSIGNED/CLI-11--bf966c07/task.md) — CLI-11: export the bus signing public key from the operator CLI (todo)
- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (in_progress)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [INVITE-CLIENT](../../INVITE/INVITE-CLIENT--4123e25d/task.md) — INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -… (todo)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (in_progress)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [RELAY-25-FU-REALHOST](../RELAY-25-FU-REALHOST--8708f7c9/task.md) — RELAY-25-FU-REALHOST: Real three-host SSH-tunnel federation run -- loopback smoke does no… (todo)
- [RELAY-26](../RELAY-26--d72a1e04/task.md) — RELAY-26: Startup refusal: non-loopback -listen with peer records and invite-gating off (todo)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (in_progress)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
