# RELAY-17: CrossBusTrust implementation + attestation travels in the relay envelope

| Field | Value |
| --- | --- |
| Public id | `817649ce-0247-4fe7-91e9-361482f2976a` |
| Key | RELAY-17 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | relay |
| Section | backlog |
| Tags | vacuous-today, critical-path |
| Created | 2026-08-08T15:56:44.357364+00:00 |
| Updated | 2026-08-08T23:19:16.573116+00:00 |
| Completed | 2026-08-08T23:19:16.573098+00:00 |

## Proof command

```sh
go test -race -run TestCrossBusTrustVerifiesAttestedEnvelope ./internal/relay
```

## Status note

Assigned after SIGN-7 release. Implement the peer-store-backed CrossBusTrust and origin-bus attestation carriage; fail closed without TOFU. Scope is released for internal/relay/message.go and signed.go, but coordinate with RELAY-12/34 committed state and do not change their records.

## Description

FEDERATION phase, wave 2. THE EPIC'S KEYSTONE.

Deps: RELAY-7 (trust deep-dive), RELAY-10 (durable peer records), RELAY-14 (attest package), AND
SIGN-7 (aeb90793) must RELEASE internal/relay/message.go and signed.go first -- filed as a real
blocking relation (SIGN-7 blocks RELAY-17), not just text, since SIGN-7 is in_progress and owns
those files right now.

Signature verification is a HARD UNAVOIDABLE DEPENDENCY, not optional hardening:
relay.ValidateRelayRequest takes CrossBusTrust as a REQUIRED parameter and nil is a refusal, so
every relayed message is ErrUnpeeredBus/403 by construction until a trust chain exists. RELAY-7/
13/14/17 are therefore ~40% of the epic and on the critical path.

~1 day of work; natural split point: (a) interface + envelope field + relay-side verification,
(b) peer-store-backed implementation.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [RELAY-10](../RELAY-10--7e9a5b63/task.md)
- **blocked by** [RELAY-14](../RELAY-14--7db695ee/task.md)
- **blocked by** RELAY-27 (unresolved)
- **blocked by** [RELAY-34](../RELAY-34--03fd8897/task.md)
- **blocked by** [RELAY-7](../RELAY-7--756655f3/task.md)
- **blocked by** [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md)
- **blocks** [RELAY-20](../RELAY-20--701dc54d/task.md)
- **blocks** [RELAY-22](../RELAY-22--b4e45cda/task.md)
- **blocks** [RELAY-23](../RELAY-23--220d36f4/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-10](../RELAY-10--7e9a5b63/task.md) — RELAY-10: Durable peer records that survive restart (done)
- [RELAY-12](../RELAY-12--069f0607/task.md) — RELAY-12: agent-bus peer add\|list\|remove (done)
- [RELAY-14](../RELAY-14--7db695ee/task.md) — RELAY-14: internal/attest: bus-signed agent-key attestations (done)
- [RELAY-7](../RELAY-7--756655f3/task.md) — RELAY-7: Cross-bus trust deep-dive (done)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [45af210c-2520-4a3a-9444-31f8e020bab1](../Reconcile-PROTOCOL.md-relay-trust-text-with-PeerStore-ba--45af210c/task.md) — Reconcile PROTOCOL.md relay-trust text with PeerStore-backed CrossBusTrust (todo)
- [48223968-0f96-4ac2-8d7e-710a1a4026b8](../Choose-the-abuse-control-primitive-for-a-MULTI-PRINCIPAL--48223968/task.md) — Choose the abuse-control primitive for a MULTI-PRINCIPAL relay link (todo)
- [RELAY-10](../RELAY-10--7e9a5b63/task.md) — RELAY-10: Durable peer records that survive restart (done)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-22](../RELAY-22--b4e45cda/task.md) — RELAY-22: Choose and wire the multi-principal relay abuse-control primitive (todo)
- [RELAY-23](../RELAY-23--220d36f4/task.md) — RELAY-23: Relay wire protocol version (todo)
- [RELAY-27](../RELAY-27--f417c6a0/task.md) — RELAY-27: fix internal/relay/signed.go:306 to wrap attest.Verify errors with %w, not %v (cancelled)
- [RELAY-27](../RELAY-27--c2486740/task.md) — RELAY-27: relay error taxonomy collapses ALL FIVE attest sentinels to ErrNoSignerKey/bad_… (done)
- [RELAY-32](../RELAY-32--23992916/task.md) — RELAY-32: add json: tags to internal/attest.Attestation before it goes on the wire (cancelled)
- [RELAY-32](../RELAY-32--c19fa33e/task.md) — RELAY-32: add json: tags to internal/attest.Attestation before it goes on the wire (todo)
- [RELAY-33](../RELAY-33--fa07908e/task.md) — RELAY-33: attest.go:371 quotes want.OriginBus unbounded (%q, 64 KiB -&gt; 262,329-byte refus… (todo)
- [RELAY-34](../RELAY-34--03fd8897/task.md) — RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can… (done)
- [RELAY-37](../RELAY-37--a613ddc8/task.md) — RELAY-37: peerstore.go:690 unparseable-URL error breaks the file's own elidePeerText(64)… (cancelled)
- [RELAY-37](../RELAY-37--7a7e6e8b/task.md) — RELAY-37: peerstore.go:690 unparseable-URL error breaks the file's own elidePeerText(64)… (todo)
- [RELAY-38](../RELAY-38--4b4beaab/task.md) — RELAY-38: signed-relay-ingest comments and docs are silent on the CodeInvalidRelay path R… (todo)
- [RELAY-7](../RELAY-7--756655f3/task.md) — RELAY-7: Cross-bus trust deep-dive (done)
- [RELAY-9-FU-CODEGUARD](../RELAY-9-FU-CODEGUARD--1e9b54d2/task.md) — RELAY-9-FU-CODEGUARD: AST guard asserting every peer error code constant has a handler ca… (todo)
- [RELAY-FU-DOCGO-CROSSBUSTRUST-STALE](../RELAY-FU-DOCGO-CROSSBUSTRUST-STALE--4988156c/task.md) — internal/relay/doc.go asserts relay ingest is structurally blocked (no CrossBusTrust impl… (todo)
- [RELAY-FU-DOCGO-GAP7-BACKOFF](../RELAY-FU-DOCGO-GAP7-BACKOFF--8aacfd4c/task.md) — internal/relay/doc.go gap 7: a fair-share or capacity refusal from AcceptRelay becomes a… (todo)
- [SIGN-1-FU-OUTOFORDER-POISON](../../SIGN/SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) — SIGN-1-FU-OUTOFORDER-POISON: Reserve-then-send lets mints be spent out of order, which pe… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
