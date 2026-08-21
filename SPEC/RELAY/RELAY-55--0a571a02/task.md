# RELAY-55: a bus can be healthy and silently deaf to the entire federation -- /healthz is not a rollout gate

| Field | Value |
| --- | --- |
| Public id | `0a571a02-2f1f-41b7-8137-1a085c30f5e1` |
| Key | RELAY-55 |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T10:09:03.131878+00:00 |
| Updated | 2026-08-21T14:14:18.149966+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestRequireFederationRefusesStartupWhenPeerSurfaceIncomplete ./cmd/agent-bus
```

## Description

# RATIFIED DECISION (2026-08-21, operator ratified in conversation; recorded by main after a downstream agent on RELAY-51 correctly refused to trust an unrecorded verbal claim — see note history for the incident this section exists to prevent)

**Ratified design:** an **authenticated `GET /v1/readyz`** (registered through `s.route`, **NOT**
added to `unauthenticatedRoutes`) **plus a runtime-gated `-require-federation` flag, default off**,
that refuses to start when peer records exist but the peer surface did not mount.

**Why this shape:** an authenticated endpoint adds nothing to invariant 3's unauthenticated
enumeration, so **no `DECISIONS.md` entry and no invariant-3 amendment is owed** by this choice.
Both alternatives considered below — a field on `/v1/info`, or an unauthenticated `/readyz` — would
have required amending that enumeration (and `/v1/info` is already double-pinned by
`healthz_info_test.go` and `durable_test.go`).

**The operator's explicit caution — this is GATING on the implementation, not decoration:**
`-require-federation` must not become another guard that cannot fire. RELAY-26 shipped a startup
refusal whose first statement tested a compile-time `const true`, so it always returned false, and
flipping that constant turns an existing test RED. **Before building the flag, establish that the
condition it refuses on is REACHABLE through supported configuration, and prove the refusal fires
with a test that observes it firing.** If it turns out unreachable, say so and propose the narrowed
form rather than shipping the wide one with a comment explaining why the dead leg is fine — that is
precisely what got `fu-relay-work` REFUSED.

**Carry RELAY-51's finding forward:** a container healthcheck **cannot authenticate**, so the
**endpoint serves the OPERATOR** and the **flag serves the ORCHESTRATOR** (fails a bad deploy at
startup, before the container is ever put in rotation). `docs/THREE-BUS-DOCKER.md` must **say which
signal is the rollout gate**, or this task will have built two signals and named neither — repeating
the exact failure mode RELAY-51 already shipped once (see the `main` response note above dated
2026-08-21, "DEAD ROLLOUT GATE").

This section is the durable record of a design that was previously ratified only in conversation.
The three-option analysis that led to it is retained below, unedited, under its own marker — do not
delete it; a flipped claim reads as freshly checked whichever way it points.

# PREMISE CORRECTIONS (verified 2026-08-21 by main, reading HEAD 1cc881f directly — not asserted)

1. **The `/healthz` body is never parsed by anything that acts on it.**
   `cmd/agent-bus/healthcheck.go` — the probe both `Dockerfile:197-198` and `docker-compose.yml:146-150`
   invoke as `CMD ["/usr/local/bin/agent-bus", "healthcheck", ...]` — drains the response body to
   `io.Discard` and branches only on `resp.StatusCode != http.StatusOK`. **Adding a field to the
   `/healthz` JSON body therefore CANNOT change the container's health verdict.** A readiness signal
   for the orchestrator has to be a different PATH (a new route/exit condition the healthcheck command
   or a sibling probe acts on) or a different EXIT CONDITION on the existing probe — never a richer
   `/healthz` body alone. This is why the ratified design above is a separate route, not a `/healthz`
   field.

2. **Two premise errors in this task's original text, already corrected elsewhere in the note
   history but not previously folded into the description:**
   - This task's body (below) inherits `internal/httpapi/peermount.go:305`'s own claim that "every
     `/v1/peer/` path answers 404" when the peer surface is incomplete. **That is wrong — it is 401.**
     With `peerRoutes` empty, `isPeerRoute` returns `false`, so the path falls through to the ordinary
     authenticated route handling and the request is refused for lacking a session, not a 404 for not
     existing. `internal/relay/client.go` never sets an `Authorization` header on the outbound peer
     handshake request (confirmed at HEAD: only `Content-Type`, `Accept` and the idempotency-key
     header are set), consistent with 401 being what a peer receives. The abandonment conclusion is
     unaffected — 401 is equally non-retriable under `internal/relay/client.go`'s `Retriable()`
     (true only for 408/429/5xx) — but the mechanism as described here was wrong.
   - This task cites `peermount.go:305`/`:319` (the two "FEDERATION IS NOT SERVED" log lines inside
     `mountPeerSurface`) as if they were the production paths. **The shipped binary reaches neither.**
     `cmd/agent-bus/main.go` passes `Peer`/`PeerPrincipals` as a **nil pair** when no peer is bindable,
     which takes `mountPeerSurface`'s silent early return ("Not even Debug: there is nothing to say
     about a surface nobody asked for") — never reaching either logged line. What production actually
     emits, at HEAD, is `cmd/agent-bus/main.go`'s `"FEDERATION IS DISABLED FOR THIS RUN: ..."` and
     `"FEDERATION INGRESS IS NOT SERVED although peering is configured: ..."` (plus a lower-case
     `"federation is not configured: ..."` info line for the ordinary non-federating case). Scoping
     this task's diagnostic work to the two `peermount.go` lines instead of the `main.go` lines would
     fix the unreachable half only — the RELAY-26 mistake in a different costume. Any implementation
     work here should key off what `main.go` actually emits/decides, not what `peermount.go` logs
     internally.

# PROOF_CMD IS A TARGET, NOT YET A PASSING COMMAND

The `proof_cmd` on this task has been narrowed from the original `go test -race ./cmd/agent-bus
./internal/relay` (which passes TODAY, before any of this exists, and therefore cannot go red for
the right reason) to name a specific negative-case test asserting the refusal/not-ready signal fires:

    go test -race -run TestRequireFederationRefusesStartupWhenPeerSurfaceIncomplete ./cmd/agent-bus

This test **does not exist yet.** It is the target proof for the `-require-federation` half of the
ratified design and is expected to be RED (or simply absent, causing `[no tests to run]`) until the
work lands — do not treat either state as a failure of this recording pass, and do not fabricate a
passing verdict. Whoever implements this must also add the `GET /v1/readyz` route's own test(s); the
named test above covers only the startup-refusal half explicitly flagged by the operator's caution.

---

FILED 2026-08-21 by main, from the RELAY-51 rollout rehearsal, which observed it directly.

# The defect

`httpapi.mountPeerSurface` is ALL-OR-NOTHING. If the `PeerSurface` is incomplete, NO peer route is
registered -- and every `/v1/peer/` path, relay included, answers 404. A 404 is non-retriable
(`internal/relay/client.go:78-84` -- Retriable() is true only for 408/429/5xx), so a sender ABANDONS
rather than retries.

MEANWHILE THE CONTAINER HEALTHCHECK STILL REPORTS `healthy`.

The only signal is a startup line at level=error: `FEDERATION IS NOT SERVED`.

# Why this matters beyond one misconfiguration

RELAY-51 needed a rollout gate -- something an operator can check between deploy stages to confirm a
bus is ready. `/healthz` is the obvious candidate and IT IS WORTHLESS FOR THIS: it stays green on a
bus that cannot participate in federation at all. A rollout gated on health would green-light a stage
that silently drops every cross-bus message.

The workaround RELAY-51 adopted is to gate on the ABSENCE of the error line, which means parsing logs
-- fragile, and not something a healthcheck can do.

# OPTIONS WEIGHED BEFORE RATIFICATION (retained for history)

Surface federation readiness where an operator and a container orchestrator can both see it. Options
worth weighing rather than assuming:
  - a separate readiness endpoint distinct from liveness (the k8s split), so /healthz keeps meaning
    "the process is up" and readiness means "it can do its job"
  - a field on GET /v1/info, which already reports build facts
  - refusing to START when peer records exist but the peer surface cannot mount -- note this is
    adjacent to RELAY-26, which was just re-scoped for shipping a startup refusal that could not fire;
    read that ruling before adding another

Whichever is chosen, the DEPLOYMENT docs must say which signal is the rollout gate.

[RATIFIED 2026-08-21 -- see "# RATIFIED DECISION" at the top of this description: the third option
plus the first, combined (authenticated GET /v1/readyz + -require-federation startup refusal), not
the second.]

# Acceptance
  - a bus that cannot serve the peer surface is distinguishable from a healthy one WITHOUT parsing logs
  - docs/THREE-BUS-DOCKER.md names the gate explicitly
  - prove the FAILURE case: stand up a bus with an incomplete peer surface and show the new signal
    reports it. A readiness signal whose negative case was never observed is not evidence.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-26](../RELAY-26--d72a1e04/task.md) — RELAY-26: Startup refusal: non-loopback -listen with peer records and invite-gating off (todo)
- [RELAY-51](../RELAY-51--0135d297/task.md) — RELAY-51: RELAY-23 rollout -- a PARTIAL deploy of the wire-version field abandons message… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [9a02d65a-e96b-4fbe-93cf-846d8b5c2034](../../DOCS/Invariant-3-s-unauthenticated-route-enumeration-is-stale--9a02d65a/task.md) — Invariant 3's unauthenticated-route enumeration is stale in three docs -- six entries in… (todo)
- [dd2cdc20-8920-4e5b-bf0a-668f439cc3a6](../../UNASSIGNED/Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) — Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS,… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
