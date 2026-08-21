# RELAY-45-FU-CLI: operator CLI surface for the inbound peer client-certificate binding

| Field | Value |
| --- | --- |
| Public id | `b9d645be-0849-4a62-9c50-3ab32e41fc8a` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | federation, mtls, cli, critical-path |
| Created | 2026-08-14T12:42:19.032310+00:00 |
| Updated | 2026-08-16T10:35:12.541081+00:00 |
| Completed | 2026-08-16T10:35:12.541064+00:00 |

## Proof command

```sh
go test -race -count=1 -run 'TestPeerAddBindsInboundClientCertFingerprint|TestPeerAddPeerClientFingerprintRequiresASigningKey|TestPeerAddNeverSilentlyErasesAnInboundBinding|TestPeerListReportsTheInboundClientCertBinding' ./cmd/agent-bus
```

## Description

RELAY-45 (4be32336-5a48-410e-a70c-62ea154a6196) shipped the durable field, validator, PutTrust collision guard and the fail-closed HTTP resolution gate for binding an inbound peer-bus TLS client certificate to the adjacent bus principal (BusTrustRecord.PeerClientTLSCertFingerprint / JSON peer_client_tls_cert_sha256, relay.ParsePeerClientTLSFingerprint, PeerStore.InboundPeerPrincipal, httpapi.RequirePeerPrincipal). It is gated (security PASS; reviewer CHANGES-REQUIRED across two rounds, re-verified complete within its stated file-ownership boundary). What is MISSING, and what RELAY-45 cannot be marked done without, is the operator/CLI half of its own acceptance criteria 2 and 5. This task delivers that half. Invariant 7 applies in full: every capability ships its CLI subcommand and AGENT_PROTOCOL.md entry in the SAME task -- nobody hand-writes HTTP, the compiled Go CLI is THE client.

SCOPE / ACCEPTANCE, verbatim from the reviewer and security gate findings on RELAY-45:

1. Add a flag to cmd/agent-bus/peer.go's `peer add` (or an equivalent subcommand) that writes BusTrust.PeerClientTLSCertFingerprint, with --json output and stable documented exit codes, matching the existing peer.go subcommand conventions.

2. The flag MUST parse the operator-supplied value through the exported relay.ParsePeerClientTLSFingerprint -- never a second, ad hoc 'looks like a fingerprint' check. That function already refuses empty, non-lowercase, malformed, and the all-zero digest; do not reimplement any part of that validation in cmd/agent-bus.

3. CARRY-FORWARD BUG, MEDIUM, found independently by both the reviewer and security gates on RELAY-45: cmd/agent-bus/peer.go (~line 789-803, PutTrust call site) currently calls `store.PutTrust(relay.BusTrust{BusID: req.busID, SigningKeys: req.keys})`. PutTrust writes the WHOLE record, so once this new flag lands, a routine signing-key rotation via `peer add -signing-key` (with no new cert flag given) will SILENTLY UN-BIND any existing inbound peer certificate -- and `trustAlreadyPinned` compares signing keys only, so the operator is told `unchanged` while a transport security credential was just revoked. Fix by carrying the existing record's PeerClientTLSCertFingerprint forward on every PutTrust call site that doesn't explicitly set it, AND by including the fingerprint in the unchanged/`trustAlreadyPinned` comparison so a silent revocation is at minimum reported truthfully. Alternatively, make PutTrust take an explicit tri-state (unset / clear / set) instead of a whole-record overwrite. Direction must remain fail-closed (no spoofing risk either way), but this is a live trap the moment the flag ships and must be fixed IN this task, not filed again.

4. validate() already requires an active trust record to carry >=1 pinned signing key. A certificate-only `peer add` (cert flag given, no signing key on file or in the same call) must be REFUSED with a message naming `-signing-key` as the remedy -- not left to surface as a bare ErrInvalidPeerRecord with no actionable guidance.

5. AGENT_PROTOCOL.md and CONTRACTS-CLI.md entries for the new flag/subcommand behaviour MUST land in this SAME task (invariant 7) -- not deferred to a later documentation pass.

6. ACCEPTANCE PROOF must be a LIVE, compiled-CLI proof, not a Go-API-only test: configure the binding through the compiled `agent-bus peer` CLI, then admit a peer bus over a REAL TLS handshake presenting that certificate, and show the request resolves to the bound principal. THIS PROOF IS CURRENTLY IMPOSSIBLE and stays impossible until BOTH: (a) RELAY-20 (701dc54d) mounts a peer-authenticated route that actually calls RequirePeerPrincipal, and (b) RELAY-24 constructs a PeerStore inside the running HTTP server (nothing does today -- Options.PeerPrincipals is wired but never populated by cmd/agent-bus/main.go). This task should therefore be SEQUENCED WITH OR AFTER RELAY-20/RELAY-24 for its acceptance proof, even though the CLI flag work itself (items 1-5) has no such dependency and can land first as code-complete-but-unprovable-live, exactly as RELAY-45 itself did.

Do not broaden scope beyond the CLI surface and the carry-forward bug above -- the HTTP mounting and PeerStore construction are RELAY-20 and RELAY-24's jobs, not this task's.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-45-FU-ROTATION](../RELAY-45-FU-ROTATION--ec1c1d7c/task.md) — RELAY-45-FU-ROTATION: inbound peer client-certificate binding has no rollover overlap win… (todo)
- [dd2cdc20-8920-4e5b-bf0a-668f439cc3a6](../../UNASSIGNED/Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) — Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS,… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
