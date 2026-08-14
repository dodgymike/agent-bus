# EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listener (needs planner pass)

| Field | Value |
| --- | --- |
| Public id | `a1b628fb-8cbf-47e8-9682-034fda8636c7` |
| Key | _(null in the export)_ |
| Epic | [MTLS](../epic.md) |
| Status | superseded |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T20:49:17.014498+00:00 |
| Updated | 2026-08-08T10:29:33.524996+00:00 |
| Completed | — |

## Status note

SUPERSEDED 2026-08-07 by the real MTLS epic (public_id f9fa37c9-85dd-4fe1-8439-543a2c4ee117, key=MTLS), created via POST /epics now that the Spec Server API supports first-class epics. This placeholder task existed to hold the epic description until a planner pass broke it into atomic tasks -- that pass already happened (2026-08-02, planner, recorded in this tasks own notes): MTLS-DESIGN, MTLS-BUSCERT, MTLS-LISTENER, MTLS-CLIENTAUTH, MTLS-BIND, MTLS-CROSSCHECK, MTLS-PIN (all now epic_key=MTLS), plus MTLS-CLIENTCERT/MTLS-VERIFY/MTLS-RELAYGUARD (P1/P2, not in my mutation scope this pass, still need epic_key=MTLS set by whoever owns them next). The breakdown IS complete; there is nothing left for this placeholder to gate. Superseded rather than done because this task itself never delivered anything -- it stood in for an epic that now exists as a first-class object. Note the DESIGN sub-question is only PARTIALLY settled: MTLS-DESIGN itself is left todo/open this pass because DECISIONS.md answers E2-E8 (fingerprint delivery, rotation, bootstrap, revocation, no-plaintext-hatch, non-loopback binding) but never states client-certificate validity-period/expiry policy and never uses the literal string MTLS-DESIGN.

## Description

USER DECISION, 2026-08-02 (DECISIONS.md "Five decisions" #5; CLAUDE.md invariant 11, amended). Supersedes the three-option "BLOCKED ON USER DECISION" framing in 0c8dc0aa-2cc2-4431-bdbf-ec5e44f3c308 -- the user has now DECIDED, that task is being corrected in the same pass to point here rather than sit with a stale open-question framing.

THE DECISION: SELF-SIGNED certificates, MUTUAL TLS, NO certificate authority anywhere. Both ends present and verify a certificate.
- Trust is established at ENROLMENT (the trust-establishing moment the design already needed): the agents client-certificate fingerprint is bound to its server-minted agent id, and the client pins the buss certificate fingerprint. This reuses the TOFU machinery the design already needs rather than inventing a second trust model -- a bus runs on a laptop with no CA in the picture.
- mTLS does NOT replace the session token -- BOTH are required, and they do DIFFERENT jobs. mTLS proves which key holder is on the connection; the session token is the revocable, time-bounded application credential -- revocability is exactly what a bare certificate lacks without a CRL.
- CROSS-CHECK REQUIRED: a session token presented over a connection whose client certificate belongs to a DIFFERENT agent must be REJECTED. This is a stronger property than either mechanism alone and is free once both exist -- do not let one silently substitute for the other.
- NEW INVARIANT 11 (CLAUDE.md, read in full before design): TLS is the required transport, there is no plaintext listener, and the server REFUSES TO START rather than fall back to plaintext. The loopback default (-listen 127.0.0.1:8080) stays but BOUNDS exposure, it does not replace TLS -- a bus deliberately exposed on a real interface needs both.

NEVER WRITE OUR OWN CRYPTO (CLAUDE.md invariant 9, absolute, outranks stdlib-first). The implementation MUST use Go stdlib crypto/tls for the handshake/transport and an audited library for anything cert-generation-adjacent that crypto/tls itself does not cover -- no hand-rolled handshake, padding, nonce or certificate-parsing logic under any circumstance.

INTERACTIONS TO DESIGN AROUND, NOT ASSUME:
- Composes with the invite-only-enrolment epic (filed separately, 2026-08-02): the invite is what AUTHORISES binding a NEW client certificate to a NEW agent id in the first place -- invite redemption and cert binding happen together.
- DEPLOY-1 (fa0c5a4e, Dockerfile) and DEPLOY-2 (14f8ec3b, docker-compose.yml): both currently assume a plaintext listener; cert/key provisioning and the compose healthcheck need to account for TLS (e.g. a healthcheck cannot curl plaintext against a TLS-only listener).
- The relay plane: invariant 2s cross-bus <bus-id>.<agent-id> addressing and loop-prevention (traversed bus path) must keep working over mTLS bus-to-bus links; every relay hop is now also a certificate-verifying TLS client and server.

NEEDS A PLANNER PASS before implementation: this is an epic, not an atomic task. A planner should break it into atomic tasks covering at minimum: self-signed cert generation + storage for the bus itself, the client-cert generation/storage story per agent, the enrolment-time fingerprint-binding + TOFU pinning flow, the crypto/tls server config (mutual auth required, no plaintext fallback -- refuse to start without valid certs per invariant 11), the session-token/client-cert cross-check, CONTRACTS-HTTP.md + PROTOCOL.md + AGENT_PROTOCOL.md updates, and paired <KEY>-DEPLOY/<KEY>-VERIFY tasks per the committed-vs-running rule since Compose/relay behaviour must be verified live, not just compiled.

Does not yet have atomic sub-tasks; do not claim-next this epic directly -- claim-next the atomic tasks a planner files under it once that pass runs.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0c8dc0aa-2cc2-4431-bdbf-ec5e44f3c308](../SUPERSEDED-by-epic-a1b628fb-8cbf-47e8-9682-034fda8636c7--0c8dc0aa/task.md) — \[SUPERSEDED by epic a1b628fb-8cbf-47e8-9682-034fda8636c7\] No transport security (TLS) any… (superseded)
- [DEPLOY-1](../../DEPLOY/DEPLOY-1--fa0c5a4e/task.md) — DEPLOY-1: Dockerfile -- multi-stage build, pinned Go builder, minimal runtime image (done)
- [DEPLOY-2](../../DEPLOY/DEPLOY-2--14f8ec3b/task.md) — DEPLOY-2: docker-compose.yml -- single bus, named volume, healthcheck (done)
- [MTLS-BIND](../MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (in_progress)
- [MTLS-BUSCERT](../MTLS-BUSCERT--93f0dc19/task.md) — MTLS-BUSCERT: generate/load the bus's self-signed certificate + private key in the data d… (done)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CLIENTCERT](../MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (in_progress)
- [MTLS-CROSSCHECK](../MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (in_progress)
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [MTLS-RELAYGUARD](../MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [MTLS-VERIFY](../MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is… (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0c8dc0aa-2cc2-4431-bdbf-ec5e44f3c308](../SUPERSEDED-by-epic-a1b628fb-8cbf-47e8-9682-034fda8636c7--0c8dc0aa/task.md) — \[SUPERSEDED by epic a1b628fb-8cbf-47e8-9682-034fda8636c7\] No transport security (TLS) any… (superseded)
- [7a197025-93f9-470b-a69b-bad494eeae94](../MTLS-re-bind-route-an-agent-renews-its-client-certificat--7a197025/task.md) — MTLS re-bind route: an agent renews its client certificate against its EXISTING agent id,… (todo)
- [MTLS-BIND](../MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (in_progress)
- [MTLS-BUSCERT](../MTLS-BUSCERT--93f0dc19/task.md) — MTLS-BUSCERT: generate/load the bus's self-signed certificate + private key in the data d… (done)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CLIENTCERT](../MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (in_progress)
- [MTLS-CROSSCHECK](../MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (in_progress)
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [MTLS-EXPIRY](../MTLS-EXPIRY--3604af80/task.md) — MTLS-EXPIRY: the client never checks the pinned bus certificate validity period -- the 36… (done)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [MTLS-RELAYGUARD](../MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [MTLS-VERIFY](../MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is… (in_progress)
- [ca356fde-0613-42cb-ac85-a629609d9c78](../Client-certificate-expiry-is-not-enforced-anywhere-Requi--ca356fde/task.md) — Client-certificate expiry is not enforced anywhere: RequireAnyClientCert does no chain ve… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
