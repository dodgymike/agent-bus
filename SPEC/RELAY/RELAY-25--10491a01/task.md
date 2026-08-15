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
| Updated | 2026-08-15T14:40:21.973277+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/fed-smoke.sh
```

## Status note

HELD in_progress by spec-keeper 2026-08-15 at HEAD 9938eb2, DESPITE `bash scripts/fed-smoke.sh` now exiting 0 (four independent reproductions; see the `main` kind=response note).

PROVEN, NOT IN DOUBT -- the epic's product objective. A->B->C, exactly once at C, ordered bus_path on all three audits, correlated by the ORIGIN's canonical content digest. All four acceptance conditions formerly in this field were RE-VERIFIED AGAINST THE COMMIT, not taken on report: (1) preflight() at fed-smoke.sh:132 names occupied 9101-9103 and stale /tmp/fed-smoke-{a,b,c}, prints manual remediation, deletes nothing (the only rm -rf are owned-bus teardown and a temp-dir RETURN trap); (2) zero-delivery classifier reads C's audit first and reports WATCH TIMEOUT/environmental for a complete [A,B,C] path (fed-smoke.sh:686-693); (3) read_audit uses the per-bus `agent-bus log --data-dir --json` SERVER binary, never agent-busctl (fed-smoke.sh:251-254); (4) reviewer/security/test-engineer/documentation/integrator gates all journaled PASS.

STILL NOT DONE -- three reasons, none of them "the smoke test fails":

1. TWO OPEN `blocks` EDGES. CRYPTO-10 (P1, todo) and SIGN-6 (P1, todo) both block this task. Not stale edges: SIGN-6 is receive-path signature verification, and fed-smoke.sh ITSELF names SIGN-6 as owning what it does not prove -- header lines 282-285: "It is a CORRELATION KEY, not a signature check. This script never verifies the Ed25519 signature, so it shows B and C recorded the same signed BYTES, not that those bytes were validly signed. SIGN-6 owns receive-path verification."

2. THIS TASK ABSORBED A SIGN-7 ACCEPTANCE ITS PROOF DOES NOT DISCHARGE. The first note here (spec-keeper, 2026-08-08) transferred it verbatim: "RELAY-25 owns the LIVE A->B->C proof: byte-preserved signature verification, exactly-once delivery at C, and durable audit path [A,B,C]." Two of three are proven. Byte PRESERVATION is proven (digest over canonical signing bytes under the origin's assignment, internal/hub/audit.go auditContentHash + signedAs gated on req.relayed at hub.go:1747). Signature VERIFICATION is not, by the script's own statement; CLAUDE.md independently records that recipients still cannot verify message signatures.

3. THE AGENT-FACING CONTRACT STILL SAYS THIS PROOF IS EXPECTED TO FAIL. At COMMITTED HEAD 9938eb2, CONTRACTS-AGENT.md:29 and :104 still describe scripts/fed-smoke.sh as failing loudly at the first unavailable step / "currently expected to fail". Verified in the commit, not the mirror; the worktree edit to that file is an unrelated CONTEXT-DOCCHECK addition and does NOT touch these lines. Completing the epic deliverable while its own shipped documentation tells agents the deliverable is expected to fail is the false-record failure CLAUDE.md forbids. Owned by RELAY-25-FU-CORRELATION-FU-AGENTDOCS (6a4f6f47-bd00-452b-ac0f-049c4b7b3ec0, P1, todo).

TO CLOSE: land AGENTDOCS (6a4f6f47); then EITHER close SIGN-6/CRYPTO-10, OR explicitly re-scope this task to exclude signature verification -- recording that SIGN-6 carries it -- and drop those two blocks edges. Do NOT complete it merely on the passing smoke run.

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
- **blocked by** [RELAY-47](../RELAY-47--dd69c4d3/task.md)
- **blocked by** [SIGN-1-FU-OUTOFORDER-POISON](../../SIGN/SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md)
- **blocked by** [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md)
- **follow-up** [RELAY-25-FU-CORRELATION](../RELAY-25-FU-CORRELATION--3f009222/task.md)
- **follow-up of** [RELAY-25-FU-INBOUNDBIND](../RELAY-25-FU-INBOUNDBIND--336c3b76/task.md)
- **relates to** [RELAY-24-BLOCKER-EGRESS-HANDSHAKE](../RELAY-24-BLOCKER-EGRESS-HANDSHAKE--0ab31d26/task.md)
- **relates to** [RELAY-24-BLOCKER-EGRESS-ATTEST](../RELAY-24-BLOCKER-EGRESS-ATTEST--3334677e/task.md)
- **relates to** [RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE](../RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE--3e96dae2/task.md)
- **relates to** [RELAY-25-FU-REALHOST](../RELAY-25-FU-REALHOST--8708f7c9/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-DOCCHECK](../../CONTEXT/CONTEXT-DOCCHECK--b3b28f45/task.md) — CONTEXT-DOCCHECK: doc-check.sh -- the instrument every other proof in this epic depends on (todo)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-25-FU-CORRELATION-FU-AGENTDOCS](../RELAY-25-FU-CORRELATION-FU-AGENTDOCS--6a4f6f47/task.md) — RELAY-25-FU-CORRELATION-FU-AGENTDOCS: CONTRACTS-AGENT.md still says fed-smoke.sh is 'expe… (todo)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

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
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md) — RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.Ne… (done)
- [RELAY-24-BLOCKER-EGRESS-ATTEST](../RELAY-24-BLOCKER-EGRESS-ATTEST--3334677e/task.md) — RELAY-24-BLOCKER-EGRESS-ATTEST: no bus can ISSUE an origin attestation for its own agents… (done)
- [RELAY-24-BLOCKER-EGRESS-HANDSHAKE](../RELAY-24-BLOCKER-EGRESS-HANDSHAKE--0ab31d26/task.md) — RELAY-24-BLOCKER-EGRESS-HANDSHAKE: this bus never DIALS a peer, so its relay Registry nev… (todo)
- [RELAY-24-BLOCKER-HUBINGEST](../RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) — RELAY-24-BLOCKER-HUBINGEST: internal/hub exported relay-ingest entry point -- foreign sen… (done)
- [RELAY-24-BLOCKER-PEERCERTFLAG](../RELAY-24-BLOCKER-PEERCERTFLAG--0e6b5a49/task.md) — RELAY-24-BLOCKER-PEERCERTFLAG: agent-bus peer add has no flag to bind a peer's inbound cl… (done)
- [RELAY-25-FU-CORRELATION](../RELAY-25-FU-CORRELATION--3f009222/task.md) — RELAY-25-FU-CORRELATION: fed-smoke.sh asserts the SAME message_id string in A's, B's and… (in_progress)
- [RELAY-25-FU-REALHOST](../RELAY-25-FU-REALHOST--8708f7c9/task.md) — RELAY-25-FU-REALHOST: Real three-host SSH-tunnel federation run -- loopback smoke does no… (todo)
- [RELAY-26](../RELAY-26--d72a1e04/task.md) — RELAY-26: Startup refusal: non-loopback -listen with peer records and invite-gating off (todo)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-47](../RELAY-47--dd69c4d3/task.md) — RELAY-47: ONWARD RELAY -- WIRE an intermediate bus to forward a relayed message to a THIR… (done)
- [SIGN-1-FU-OUTOFORDER-POISON](../../SIGN/SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) — SIGN-1-FU-OUTOFORDER-POISON: Reserve-then-send lets mints be spent out of order, which pe… (done)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (superseded)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
