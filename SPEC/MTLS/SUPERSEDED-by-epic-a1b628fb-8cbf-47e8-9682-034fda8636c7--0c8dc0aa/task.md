# \[SUPERSEDED by epic a1b628fb-8cbf-47e8-9682-034fda8636c7\] No transport security (TLS) anywhere in the server -- decision made, options list is stale

| Field | Value |
| --- | --- |
| Public id | `0c8dc0aa-2cc2-4431-bdbf-ec5e44f3c308` |
| Key | _(null in the export)_ |
| Epic | [MTLS](../epic.md) |
| Status | superseded |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T20:39:07.437628+00:00 |
| Updated | 2026-08-08T10:29:33.145243+00:00 |
| Completed | — |

## Status note

SUPERSEDED 2026-08-02: the user has DECIDED (DECISIONS.md "Five decisions" #5; CLAUDE.md invariant 11) -- self-signed certificates, MUTUAL TLS, no CA anywhere. This task listed three un-selected options and is now stale; the decision and the atomic breakdown live under the new epic a1b628fb-8cbf-47e8-9682-034fda8636c7 ("EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listener"). Do not work this task; work the epic (after its planner pass) instead.

## Description

BLOCKED ON USER DECISION -- do not start implementation. This task exists to record the gap and lay out options; a design/consent decision from the user is required before any code changes.

The server has no transport security of any kind. This is now load-bearing rather than theoretical: with no per-agent binding left on the session token, token unguessability is the ONLY thing protecting a session, and an on-path network observer can also kill a pending challenge outright (no confidentiality or integrity on that handshake). The default listen address was just moved to loopback (task c27f9439), which contains the exposure for a single-host deployment but does nothing for the Docker Compose / multi-bus relay target, where buses talk to each other over a real network -- that is precisely invariant 2's cross-bus routing, so the relay path is plaintext today.

Options to lay out for the user (none pre-selected):
1. Terminate TLS in the server itself (Go stdlib crypto/tls + net/http's ListenAndServeTLS -- this is stdlib, not 'writing your own crypto': TLS termination via crypto/tls is exactly the audited, high-level API the project's crypto rule calls for, as opposed to hand-rolling a handshake). Needs a cert/key provisioning story (self-signed for dev, ACME/reverse-proxy-issued for prod).
2. Require a reverse proxy / sidecar (e.g. Caddy, nginx, an Envoy sidecar) in front of the server and document that as the SUPPORTED deployment; the Go server stays plaintext-over-localhost/private-network only. Simplest for the single-bus case, weakest for bus-to-bus relay unless every hop also proxies.
3. Mutual TLS specifically between relaying buses (bus-to-bus federation traffic authenticates both ends via client + server certs), leaving agent<->bus traffic on option 1 or 2. Most targeted at the actual multi-bus relay risk, but adds cert lifecycle for every bus pair.

Constraints that bear on the decision:
- NEVER WRITE OUR OWN CRYPTO (absolute project rule, CLAUDE.md rule 9) -- any of the above must use crypto/tls or an equivalently audited, high-level library. No hand-rolled handshake, padding, or nonce scheme under any option.
- Invariant 3: enrolment issues a signed credential and every route except enrolment authenticates -- TLS is a complement to that credential, not a replacement; the credential-forging and replay protections stay required regardless of which TLS option is chosen.
- Invariant 2: cross-bus routing depends on unambiguous `<bus-id>.<agent-id>` addressing carried over relay hops -- whichever TLS option is chosen must not break that addressing or the relay's loop-prevention (traversed bus path).
- Exposing the bus on a non-loopback interface, and any change to authn/authz defaults, are CONSENT-GATED actions per this project's operating rules -- flagging explicitly here since options 1 and 3 both imply the server (or a sidecar in front of it) eventually listens on a non-loopback interface for the Compose/multi-bus target.

No proof_cmd yet -- there is nothing to prove until a decision is made and an implementation task is filed against it. When unblocked, split into: (a) the chosen TLS mechanism, (b) cert/key provisioning + rotation story, (c) the paired <KEY>-DEPLOY/<KEY>-VERIFY task per CLAUDE.md's committed-vs-running rule, since 'TLS code compiles' and 'a bus-to-bus relay call in Docker Compose is actually encrypted on the wire' are different claims.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1-FU-LISTENADDR](../../AUTH/AUTH-1-FU-LISTENADDR--c27f9439/task.md) — AUTH-1-FU-LISTENADDR: default listen address is :8080 (all interfaces) but DECISIONS.md s… (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
