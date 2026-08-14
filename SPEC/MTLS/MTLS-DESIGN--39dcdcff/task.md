# MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, how a client learns the bus fingerprint, rotation, expiry, and the no-plaintext-in-tests answer

| Field | Value |
| --- | --- |
| Public id | `39dcdcff-8fc5-4220-85d5-29bc52d8dd6f` |
| Key | MTLS-DESIGN |
| Epic | [MTLS](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:49.554879+00:00 |
| Updated | 2026-08-14T12:37:15.095520+00:00 |
| Completed | 2026-08-14T12:37:15.095503+00:00 |

## Proof command

```sh
grep -q '^## 2026-08-07 — MTLS-DESIGN: the consolidated certificate lifecycle' DECISIONS.md
```

## Status note

2026-08-07 update: work COMPLETE and STAGED in DECISIONS.md (new dated section "2026-08-07 -- MTLS-DESIGN: the consolidated certificate lifecycle"), NOT yet committed -- awaiting the user's commit. Flip to done via /complete with the real commit_sha once the user commits; do not flip on this note alone. This unblocks MTLS-BUSCERT, MTLS-LISTENER, MTLS-CLIENTAUTH, MTLS-BIND, MTLS-CLIENTCERT and MTLS-PIN, all of which listed MTLS-DESIGN as a dependency. The task's own description text "BLOCKED ON USER DECISION" is stale and is now resolved by the staged DECISIONS.md entry.

## Description

EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: none | BLOCKS: MTLS-BUSCERT, MTLS-LISTENER, MTLS-CLIENTAUTH, MTLS-BIND, MTLS-CLIENTCERT, MTLS-PIN

BLOCKED ON USER DECISION. DECISIONS.md:1131-1147 settles "self-signed, mutual, no CA, bound at enrolment" but leaves these open, and every one of them is load-bearing: (1) how a client learns the bus's cert fingerprint BEFORE its first connection -- the planner recommends the invite blob carry bus-id + address + bus-cert fingerprint + invite secret, which removes the TOFU window entirely and is what makes the two epics genuinely compose, versus plain TOFU-on-first-connect; (2) certificate validity period and what happens when an agent's client cert EXPIRES (re-enrol with a fresh invite, or a re-bind route); (3) bus-key rotation, which invalidates every client's pin -- accepted "operator must re-pin" event, or must the bus serve two certs during a rollover; (4) whether a plaintext escape hatch exists for tests or local dev (invariant 11 says no); (5) whether the cert/key are always self-generated or may be operator-supplied via flags. INVARIANT 9 IS ABSOLUTE: stdlib crypto/tls + crypto/x509, standard fingerprint = SHA-256 over the certificate DER. No hand-rolled fingerprint scheme, cert format, nonce or key exchange -- if a sub-task looks like it needs one, it is mis-specced; stop and escalate.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-BIND](../MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (in_progress)
- [MTLS-BUSCERT](../MTLS-BUSCERT--93f0dc19/task.md) — MTLS-BUSCERT: generate/load the bus's self-signed certificate + private key in the data d… (done)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CLIENTCERT](../MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (in_progress)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [7a197025-93f9-470b-a69b-bad494eeae94](../MTLS-re-bind-route-an-agent-renews-its-client-certificat--7a197025/task.md) — MTLS re-bind route: an agent renews its client certificate against its EXISTING agent id,… (todo)
- [MTLS-BUSCERT](../MTLS-BUSCERT--93f0dc19/task.md) — MTLS-BUSCERT: generate/load the bus's self-signed certificate + private key in the data d… (done)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CLIENTCERT](../MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (in_progress)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)
- [ca356fde-0613-42cb-ac85-a629609d9c78](../Client-certificate-expiry-is-not-enforced-anywhere-Requi--ca356fde/task.md) — Client-certificate expiry is not enforced anywhere: RequireAnyClientCert does no chain ve… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
