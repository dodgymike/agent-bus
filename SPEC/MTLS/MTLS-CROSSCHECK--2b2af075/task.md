# MTLS-CROSSCHECK: reject a session token presented over a connection whose client certificate belongs to a DIFFERENT agent

| Field | Value |
| --- | --- |
| Public id | `2b2af075-a295-4cf3-9826-b1a3554c8795` |
| Key | MTLS-CROSSCHECK |
| Epic | [MTLS](../epic.md) |
| Status | in_progress |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:50.814945+00:00 |
| Updated | 2026-08-14T20:09:26.288591+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestSessionTokenRejectedOnForeignClientCert|TestSessionBeginRejectedOnForeignClientCert|TestSessionCompleteRejectedOnForeignClientCert' ./internal/httpapi ./internal/auth && grep -qi 'cross-check' CONTRACTS-HTTP.md
```

## Description

EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-BIND | BLOCKS: MTLS-VERIFY, AUTH-2-FU-POLLEXPIRY (03d7ca66)

**THE PART MOST LIKELY TO BE QUIETLY OMITTED -- the user called this out by name. DO NOT fold it into MTLS-BIND and do not complete either task on the other's tests.** CLAUDE.md invariant 11 and DECISIONS.md:1139-1144: mTLS does NOT replace the session token; BOTH are required and they must be CROSS-CHECKED. mTLS proves which key holder is on the connection; the session token is the revocable, time-bounded application credential. Three call sites, all of which must be covered: (1) (*Server).authMiddleware (internal/httpapi/authmw.go:241, which calls s.auth.Authenticate at :277 and attaches the auth.Principal at :299) must compare the connection's peer-cert fingerprint against the fingerprint bound to principal.AgentID; (2) POST /v1/session/begin (internal/httpapi/auth.go:172) takes an agent_id from an unauthenticated body -- a begin naming agent X over agent Y's certificate must be refused; (3) POST /v1/session/complete (auth.go:211) re-reads the roster entry at internal/auth/session.go:344. NOTE httpapi.Options.Auth is the CONCRETE *auth.Service (internal/httpapi/server.go:108), not an interface, so this needs either a new method (e.g. AuthenticateBound(token, fingerprint)) or a new interface seam. A mismatch is a protocol violation, not a routine 401 -- log it as security. Also record in this task that AUTH-2-FU-POLLEXPIRY (03d7ca66) must re-evaluate the cross-check mid-poll, not only at request entry.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-2-FU-POLLEXPIRY](../../AUTH/AUTH-2-FU-POLLEXPIRY--03d7ca66/task.md) — AUTH-2-FU-POLLEXPIRY: A long-poll can outlive its session, quietly contradicting immediat… (todo)
- [MTLS-BIND](../MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (in_progress)
- [MTLS-VERIFY](../MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is… (in_progress)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-2-FU-POLLEXPIRY](../../AUTH/AUTH-2-FU-POLLEXPIRY--03d7ca66/task.md) — AUTH-2-FU-POLLEXPIRY: A long-poll can outlive its session, quietly contradicting immediat… (todo)
- [MTLS-BIND](../MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (in_progress)
- [MTLS-BIND-FU-DOCS](../MTLS-BIND-FU-DOCS--8c40ea26/task.md) — MTLS-BIND-FU-DOCS: document the enrolment certificate binding -- CONTRACTS-HTTP.md 409, D… (todo)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [RELAY-45](../../RELAY/RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-FU-PEERBUSID-CROSSCHECK](../../RELAY/RELAY-FU-PEERBUSID-CROSSCHECK--b2c28232/task.md) — RELAY-FU-PEERBUSID-CROSSCHECK: invariant 11's PEER cross-check is documented but unimplem… (todo)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)
- [ca356fde-0613-42cb-ac85-a629609d9c78](../Client-certificate-expiry-is-not-enforced-anywhere-Requi--ca356fde/task.md) — Client-certificate expiry is not enforced anywhere: RequireAnyClientCert does no chain ve… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
