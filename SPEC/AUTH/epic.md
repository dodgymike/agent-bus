# EPIC AUTH — Enrolment & authentication

[← all epics](../../SPEC.md)

**16 open / 23 total.** Full records live in `SPEC/AUTH/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (16)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| 1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa | SEC: unauthenticated enrol permanently bricks the roster -- 4096-cap fails closed forever… | todo | P0 | [task.md](SEC-unauthenticated-enrol-permanently-bricks-the-roster--1c4d3dea/task.md) | — | [AUTH-3](AUTH-3--d53e3b21/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) [AUTH-4](AUTH-4--a853261d/task.md) |
| AUTH-3 | AUTH-3: Roster persistence & recovery | in_progress | P0 | [task.md](AUTH-3--d53e3b21/task.md) | blocks [LIVE-5](../LIVE/LIVE-5--7f62eeee/task.md) | [ID-3](../ID/ID-3--745cce13/task.md) [ID-2-WIRING-OBSERVER](../ID/ID-2-WIRING-OBSERVER--c31f6999/task.md) [ENROL-SHAPE](../INVITE/ENROL-SHAPE--8942c8c8/task.md) |
| AUTH-ROSTER-RECLAIM | AUTH-ROSTER-RECLAIM: operator-side "agent-bus roster remove &lt;id&gt;" escape hatch -- filesys… | todo | P0 | [task.md](AUTH-ROSTER-RECLAIM--b418638c/task.md) | — | [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) [AUTH-4](AUTH-4--a853261d/task.md) [1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa](SEC-unauthenticated-enrol-permanently-bricks-the-roster--1c4d3dea/task.md) |
| AUTH-1-FU-ACTIVECAP-DOCS | AUTH-1-FU-ACTIVECAP-DOCS: document the per-agent ACTIVE-session cap in CONTRACTS-HTTP.md… | todo | P1 | [task.md](AUTH-1-FU-ACTIVECAP-DOCS--27a811c9/task.md) | — | [AUTH-1-FU-ACTIVECAP](AUTH-1-FU-ACTIVECAP--2d92b699/task.md) [AUTH-1-FU-RATELIMIT](AUTH-1-FU-RATELIMIT--42670f8b/task.md) [AUTH-1-FU-PENDINGCAP](AUTH-1-FU-PENDINGCAP--687ad8c9/task.md) [0b43393e-556b-409a-938a-846be2fb4a75](../INVITE/EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) |
| AUTH-1-FU-ACTIVECAP-RETRYAFTER | AUTH-1-FU-ACTIVECAP-RETRYAFTER: a per-agent cap 503 tells the client the wrong thing and… | todo | P1 | [task.md](AUTH-1-FU-ACTIVECAP-RETRYAFTER--03a8512b/task.md) | — | [AUTH-1-FU-ACTIVECAP](AUTH-1-FU-ACTIVECAP--2d92b699/task.md) |
| AUTH-1-FU-POPKEY | AUTH-1-FU-POPKEY: enrolment does not prove possession of the enrolling private key | todo | P1 | [task.md](AUTH-1-FU-POPKEY--6e3083b0/task.md) | — | [AUTH-1](AUTH-1--54fa94c0/task.md) |
| AUTH-1-FU-RATELIMIT | AUTH-1-FU-RATELIMIT: per-source rate limiting on the three unauthenticated auth routes | todo | P1 | [task.md](AUTH-1-FU-RATELIMIT--42670f8b/task.md) | — | [AUTH-1](AUTH-1--54fa94c0/task.md) |
| AUTH-1-FU-SESSIONSCALE | AUTH-1-FU-SESSIONSCALE: session-table O(n) scans and refuse-not-evict policy cause CPU/lo… | todo | P1 | [task.md](AUTH-1-FU-SESSIONSCALE--067b80cf/task.md) | — | [AUTH-1](AUTH-1--54fa94c0/task.md) [AUTH-3](AUTH-3--d53e3b21/task.md) |
| AUTH-2-FU-POLLEXPIRY | AUTH-2-FU-POLLEXPIRY: A long-poll can outlive its session, quietly contradicting immediat… | todo | P1 | [task.md](AUTH-2-FU-POLLEXPIRY--03d7ca66/task.md) | blocks [AUTH-4](AUTH-4--a853261d/task.md) | [AUTH-2](AUTH-2--4b45a6d8/task.md) [AUTH-4](AUTH-4--a853261d/task.md) [MTLS-CROSSCHECK](../MTLS/MTLS-CROSSCHECK--2b2af075/task.md) |
| AUTH-3-FU-ROSTERDOS-DOCS | AUTH-3-FU-ROSTERDOS-DOCS: extend session.go availability analysis (untargeted/unamplified… | todo | P1 | [task.md](AUTH-3-FU-ROSTERDOS-DOCS--d5197abb/task.md) | — | [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) [1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa](SEC-unauthenticated-enrol-permanently-bricks-the-roster--1c4d3dea/task.md) |
| AUTH-4 | AUTH-4: POST /v1/leave -- leave / revocation | todo | P1 | [task.md](AUTH-4--a853261d/task.md) | blocked by [AUTH-2-FU-POLLEXPIRY](AUTH-2-FU-POLLEXPIRY--03d7ca66/task.md) | [ID-3](../ID/ID-3--745cce13/task.md) [AUTH-1](AUTH-1--54fa94c0/task.md) |
| AUTH-5 | AUTH-5: Auth crash/recovery test | todo | P1 | [task.md](AUTH-5--2b0791d4/task.md) | — | — |
| MSG-FU-ROSTERSOURCE | MSG-FU-ROSTERSOURCE: the hub must read the AUTHORITATIVE roster the moment AUTH-3 makes e… | todo | P1 | [task.md](MSG-FU-ROSTERSOURCE--fa26036c/task.md) | — | [AUTH-3](AUTH-3--d53e3b21/task.md) |
| MTLS-VERIFY-FU | MTLS-VERIFY-FU-DOCSCHEME (README/PROTOCOL half): main still documents the bus as plaintex… | todo | P1 | [task.md](MTLS-VERIFY-FU--5f8e0cba/task.md) | blocks [HANDOVER-README](../HANDOVER/HANDOVER-README--1dc9cf90/task.md) | [MTLS-LISTENER](../MTLS/MTLS-LISTENER--17e70a7e/task.md) [MTLS-VERIFY-FU-DOCSCHEME](../DOCS/MTLS-VERIFY-FU-DOCSCHEME--cb4fd330/task.md) |
| AUTH-2-FU-SESSMU | AUTH-2-FU-SESSMU: auth.Service.Authenticate now takes an exclusive mutex on every request… | todo | P2 | [task.md](AUTH-2-FU-SESSMU--160b765b/task.md) | — | [AUTH-2](AUTH-2--4b45a6d8/task.md) |
| ac4f9c2b-5460-4e83-997d-0e433194752f | Enrol accepts a duplicate enrolment public key -- one keypair can hold unlimited agent ids | todo | P2 | [task.md](Enrol-accepts-a-duplicate-enrolment-public-key-one-keypa--ac4f9c2b/task.md) | — | [AUTH-1-FU-ACTIVECAP](AUTH-1-FU-ACTIVECAP--2d92b699/task.md) [AUTH-4](AUTH-4--a853261d/task.md) |

## Closed tasks (7) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| AUTH-1 | AUTH-1: POST /v1/enroll -- signed credential issuance | done | P0 | [task.md](AUTH-1--54fa94c0/task.md) | — | [AGENTIF-2](../AGENTIF/AGENTIF-2--15e4509c/task.md) [CLI-2](../CLI/CLI-2--39318208/task.md) [ID-3](../ID/ID-3--745cce13/task.md) [CRYPTO-3](../CRYPTO/CRYPTO-3--dd1066af/task.md) [AUTH-3](AUTH-3--d53e3b21/task.md) [AUTH-2](AUTH-2--4b45a6d8/task.md) +6 more |
| AUTH-1-FU-ACTIVECAP | AUTH-1-FU-ACTIVECAP: cap ACTIVE sessions per agent -- the one place an agent-id-keyed cap… | done | P0 | [task.md](AUTH-1-FU-ACTIVECAP--2d92b699/task.md) | — | [AUTH-1-FU-PENDINGCAP](AUTH-1-FU-PENDINGCAP--687ad8c9/task.md) [AUTH-1-FU-ACTIVECAP-DOCS](AUTH-1-FU-ACTIVECAP-DOCS--27a811c9/task.md) |
| AUTH-1-FU-LISTENADDR | AUTH-1-FU-LISTENADDR: default listen address is :8080 (all interfaces) but DECISIONS.md s… | done | P0 | [task.md](AUTH-1-FU-LISTENADDR--c27f9439/task.md) | — | [AUTH-1](AUTH-1--54fa94c0/task.md) [LISTENADDR-FU-CONTRACTS](../DOCS/LISTENADDR-FU-CONTRACTS--b0a5630b/task.md) |
| AUTH-1-FU-PENDINGCAP | AUTH-1-FU-PENDINGCAP: MaxPendingPerAgent is a lockout primitive, not a defence -- rekey o… | done | P0 | [task.md](AUTH-1-FU-PENDINGCAP--687ad8c9/task.md) | — | [AUTH-1](AUTH-1--54fa94c0/task.md) |
| AUTH-2 | AUTH-2: Token verification middleware | done | P0 | [task.md](AUTH-2--4b45a6d8/task.md) | — | — |
| AUTH-6 | AUTH-6: Auth FAIL-OPEN risk -- wrap the mux with auth + an explicit unauthenticated allow… | superseded | P1 | [task.md](AUTH-6--1640e0b4/task.md) | — | [CORE-1](../CORE/CORE-1--eea035e4/task.md) [CORE-4](../CORE/CORE-4--d9ffbf08/task.md) [AUTH-2](AUTH-2--4b45a6d8/task.md) [CORE-8](../CORE/CORE-8--1e9dae04/task.md) |
| AUTH-2-FU-RATELIMIT | AUTH-2-FU-RATELIMIT: Rate-limit the unauthenticated routes and the 401 path | superseded | P2 | [task.md](AUTH-2-FU-RATELIMIT--504caef3/task.md) | — | [AUTH-2](AUTH-2--4b45a6d8/task.md) [AUTH-1-FU-RATELIMIT](AUTH-1-FU-RATELIMIT--42670f8b/task.md) |

## Epic description

Agent submits a key, server signs it and returns a bearer token; token verification middleware; revocation/leave; roster persistence (invariant 3).

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
