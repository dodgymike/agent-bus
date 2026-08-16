# RELAY-25-FU-INBOUNDBIND: fed-smoke.sh never binds each peer's INBOUND client-certificate fingerprint

| Field | Value |
| --- | --- |
| Public id | `336c3b76-6fab-478e-af3d-af99dde597fd` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | relay |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T07:23:14.914416+00:00 |
| Updated | 2026-08-15T07:50:40.201219+00:00 |
| Completed | 2026-08-15T07:50:34.485649+00:00 |

## Proof command

```sh
rm -rf /tmp/fed-smoke-a /tmp/fed-smoke-b /tmp/fed-smoke-c; bash scripts/fed-smoke.sh >/dev/null 2>&1; grep -q "FEDERATION INGRESS is served" /tmp/fed-smoke-a/run/agent-bus.log && grep -q "bindable_peers=1 " /tmp/fed-smoke-a/run/agent-bus.log && grep -q "FEDERATION INGRESS is served" /tmp/fed-smoke-b/run/agent-bus.log && grep -q "bindable_peers=2 " /tmp/fed-smoke-b/run/agent-bus.log && grep -q "FEDERATION INGRESS is served" /tmp/fed-smoke-c/run/agent-bus.log && grep -q "bindable_peers=1 " /tmp/fed-smoke-c/run/agent-bus.log && ! grep -l "level=error" /tmp/fed-smoke-a/run/agent-bus.log /tmp/fed-smoke-b/run/agent-bus.log /tmp/fed-smoke-c/run/agent-bus.log   # Pins the ACHIEVED claim (all 3 bus logs carry FEDERATION INGRESS is served with the exact bindable_peers value per bus -- A=1,B=2,C=1 -- and zero level=error lines), not the end-to-end smoke test (which still exits 7 on the SEPARATE unwired-egress defect, RELAY-24-BLOCKER-EGRESS 85ae8b32). Verified by spec-keeper via bash scripts/proof-check.sh: RED against scripts/fed-smoke.sh @bfbf4e7 (pre-RELAY-25-FU-INBOUNDBIND) -- exit 1, all three logs instead carry level=error "FEDERATION IS NOT SERVED although peering is configured"; GREEN against HEAD (1753121, which contains 4dd5b67) -- exit 0.
```

## Status note

DONE. Ingress-side defect closed (4dd5b67, amended to 7095231): the blocker (RELAY-24 composition-root wiring, e303c624) landed, so bindablePeerCount() has a production caller and this bindings work is no longer inert. Verified RED (pre-fix script) / GREEN (HEAD) by spec-keeper. Remaining unwired egress tracked separately at RELAY-24-BLOCKER-EGRESS (85ae8b32).

## Description

scripts/fed-smoke.sh configures peering with `peer add -tls-fingerprint` only, which is the OUTBOUND next-hop server certificate (PeerRecord.NextHopTLSCertFingerprint, address-keyed). It never passes `-peer-client-fingerprint`, which is the INBOUND binding (BusTrustRecord.PeerClientTLSCertFingerprint, bus-principal-keyed) shipped by commit a413ecc under RELAY-24-BLOCKER-PEERCERTFLAG. Because relaywiring.go's bindablePeerCount() gates the federation ingress on that binding being present, all three buses log "FEDERATION IS NOT SERVED although peering is configured" at startup and register no peer route.

Scope is scripts/fed-smoke.sh ONLY: pass the inbound binding in add_trust, sourced from the peer's own `invite mint -json` bus_cert_fingerprint (already captured by the script as fp_a/fp_b/fp_c) -- no certificate/key-file scraping, which the script header forbids.

Explicitly OUT OF SCOPE: cmd/agent-bus/main.go, which is owned by the in_progress RELAY-24 (Composition root) and currently carries ~163 uncommitted lines including the false remedy string.

Known bound: this does NOT make the smoke test pass. /v1/send returns 404 "unknown recipient" because relay.NewForwarder has no production caller (no egress). This task only clears the INGRESS gate so peer routes register.

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
- [RELAY-24-BLOCKER-PEERCERTFLAG](../RELAY-24-BLOCKER-PEERCERTFLAG--0e6b5a49/task.md) — RELAY-24-BLOCKER-PEERCERTFLAG: agent-bus peer add has no flag to bind a peer's inbound cl… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24-BLOCKER-EGRESS-HANDSHAKE](../RELAY-24-BLOCKER-EGRESS-HANDSHAKE--0ab31d26/task.md) — RELAY-24-BLOCKER-EGRESS-HANDSHAKE: this bus never DIALS a peer, so its relay Registry nev… (todo)
- [db3047c0-13dd-4885-8f8f-5d86adf023ee](../fed-smoke.sh-add_route-add_trust-discard-stdout-and-neve--db3047c0/task.md) — fed-smoke.sh: add_route/add_trust discard stdout and never require_ok, so a dropped finge… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
