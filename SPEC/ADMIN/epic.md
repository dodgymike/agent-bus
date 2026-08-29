# EPIC ADMIN — ADMIN: the local operator console (agent-busadm)

[← all epics](../../SPEC.md)

**14 open / 14 total.** Full records live in `SPEC/ADMIN/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (14)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| ADMIN-1 | ADMIN-1: record the operator-console trust/transport/control rulings D1-D7 in DECISIONS.m… | blocked | P2 | [task.md](ADMIN-1--db334b3c/task.md) | — | [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) [ADMIN-9](ADMIN-9--8bb10db2/task.md) [ADMIN-10](ADMIN-10--958d66e8/task.md) [ADMIN-11](ADMIN-11--07926508/task.md) [AUTH-4](../AUTH/AUTH-4--a853261d/task.md) |
| ADMIN-2 | ADMIN-2: client.Info/Health/Discovery + \`agent-busctl status \[--json\]\`, shipped together… | todo | P2 | [task.md](ADMIN-2--786e0de1/task.md) | — | [ADMIN-1](ADMIN-1--db334b3c/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |
| ADMIN-3 | ADMIN-3: \`agent-busadm serve\` -- loopback-only console with a capability token and an emb… | todo | P2 | [task.md](ADMIN-3--76bfce36/task.md) | relates to [TUI-1](../TUI/TUI-1--3ea68265/task.md) | [ADMIN-1](ADMIN-1--db334b3c/task.md) [ADMIN-2](ADMIN-2--786e0de1/task.md) [ADMIN-4](ADMIN-4--e12b4149/task.md) [ADMIN-C2](ADMIN-C2--d31d77ff/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |
| ADMIN-4 | ADMIN-4: N buses from a config file, polled concurrently -- one hung bus must not stall t… | todo | P2 | [task.md](ADMIN-4--e12b4149/task.md) | — | [ADMIN-3](ADMIN-3--76bfce36/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |
| ADMIN-5 | ADMIN-5: roster + live flow view from the console's OWN long-poll (/v1/wait) -- metadata… | todo | P2 | [task.md](ADMIN-5--fc0b4a88/task.md) | — | [ADMIN-4](ADMIN-4--e12b4149/task.md) [ADMIN-6](ADMIN-6--f92aa33f/task.md) [ADMIN-7](ADMIN-7--2147523d/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |
| ADMIN-6 | ADMIN-6: bounded, tail-tolerant STREAMING audit reader in internal/wal (no dir lock, torn… | todo | P2 | [task.md](ADMIN-6--f92aa33f/task.md) | — | [ADMIN-1](ADMIN-1--db334b3c/task.md) [ADMIN-7](ADMIN-7--2147523d/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |
| ADMIN-10 | ADMIN-10: online invite mint from the console (BLOCKED -- ruled out for now by D6; filed… | blocked | P3 | [task.md](ADMIN-10--958d66e8/task.md) | — | [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |
| ADMIN-11 | ADMIN-11: remove an agent from the console (BLOCKED on AUTH-4) | blocked | P3 | [task.md](ADMIN-11--07926508/task.md) | — | [AUTH-4](../AUTH/AUTH-4--a853261d/task.md) [ADMIN-4](ADMIN-4--e12b4149/task.md) [ADMIN-C3](ADMIN-C3--ca0653e3/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |
| ADMIN-7 | ADMIN-7: audit view in the console, for a CO-LOCATED bus only (D5) | todo | P3 | [task.md](ADMIN-7--2147523d/task.md) | — | [ADMIN-6](ADMIN-6--f92aa33f/task.md) [ADMIN-4](ADMIN-4--e12b4149/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |
| ADMIN-8 | ADMIN-8: GET /v1/status -- authenticated, in-process counters, exhaustive field-set pin,… | todo | P3 | [task.md](ADMIN-8--7f550309/task.md) | relates to [TUI-4](../TUI/TUI-4--11898d9b/task.md)<br>supersedes [CORE-5](../CORE/CORE-5--06c5b1f5/task.md) | [ADMIN-2](ADMIN-2--786e0de1/task.md) [CORE-5](../CORE/CORE-5--06c5b1f5/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |
| ADMIN-9 | ADMIN-9: the console enrols by redeeming an invite blob (BLOCKED on INVITE-GATE) | blocked | P3 | [task.md](ADMIN-9--8bb10db2/task.md) | — | [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) [ADMIN-3](ADMIN-3--76bfce36/task.md) [ADMIN-4](ADMIN-4--e12b4149/task.md) |
| ADMIN-C1 | ADMIN-C1: versioned control/telemetry schema in a new internal/adminctl -- unknown kinds… | todo | P3 | [task.md](ADMIN-C1--9074f7f2/task.md) | — | [ADMIN-1](ADMIN-1--db334b3c/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |
| ADMIN-C2 | ADMIN-C2: \`agent-busctl report\` -- the node reporter: allow-list check, refuse-with-reaso… | todo | P3 | [task.md](ADMIN-C2--d31d77ff/task.md) | — | [ADMIN-C1](ADMIN-C1--9074f7f2/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |
| ADMIN-C3 | ADMIN-C3: console issues/renews telemetry leases and renders the stream -- A REFUSAL MUST… | todo | P3 | [task.md](ADMIN-C3--ca0653e3/task.md) | — | [ADMIN-C2](ADMIN-C2--d31d77ff/task.md) [ADMIN-4](ADMIN-4--e12b4149/task.md) [ADMIN-2](ADMIN-2--786e0de1/task.md) [ADMIN-8](ADMIN-8--7f550309/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) |

## Closed tasks (0) — done, cancelled, superseded

_None._

## Epic description

The local operator console for a set of agent-bus nodes. Ships as a NEW BINARY `agent-busadm`
that runs on the operator's own machine, holds the operator's bus credentials, speaks pinned mutual TLS to each
bus ONLY through the `client/` package, and serves a read-first UI over loopback so that no browser ever
terminates a pinned connection. Monitoring is built from what the system already records -- liveness, identity,
roster, flow, volume, topology, integrity -- and NEVER message bodies (invariant 6). Control is consent-based:
a node is ASKED, over the ordinary authenticated message path, to stream telemetry for a bounded lease, and may
refuse.

EXPLICITLY NOT COVERED: message bodies; any role or privilege tier inside the bus; peering/federation/relay
configuration; multi-user auth, remote administration or internet exposure; replacing `agent-busctl`; any write
to the WAL or audit log; an in-bus scheduler.

## Operator rulings (D1-D7) -- these are DECIDED, not open questions

- D1 -- UI TRANSPORT: plaintext HTTP on loopback + a per-process capability token, strict `Origin` /
  `Sec-Fetch-Site` checks, plus a 0600 unix socket for non-browser access. Ruled EXPLICITLY, not defaulted:
  this is a console surface, not a bus surface, and TLS here would reintroduce the browser trust-store problem
  the architecture exists to eliminate.
- D2 -- THE REPORTER IS AN `agent-busctl report` SUBCOMMAND, not a third binary.
- D3 -- ADVISORY LEASE. Bounded, expires, does NOT survive restart. Cost accepted: if the console is down,
  telemetry stops. That is fail-closed on a control channel, and a stolen console credential buys an attacker
  only until the lease expires. Durable standing configuration was EXPLICITLY REJECTED -- it would need a new
  durable record type, let a remote party permanently alter a node's behaviour, and make revocation a
  distributed problem.
- D4 -- AUTHORISATION IS A LOCAL ALLOW-LIST of admin agent ids per node. No new bus authority, no new route, no
  role tier. This is literally the "configured to allow it" in the request.
- D5 -- NO REMOTE AUDIT READING. Co-located filesystem read only; the audit log is the bus's complete social
  graph, and reading it stays an operator/filesystem capability exactly as invite-minting already is.
- D6 -- NO ONLINE INVITE MINT. `agent-bus invite mint` takes the exclusive dirlock and needs the bus stopped;
  the console links to the command instead.
- D7 -- YES, reserve `admin-control-schema` (done: namespace `admin-control-schema`, value 1).

## Epic-wide constraints (recorded here, not buried in one task)

- NOTHING HERE IS BLOCKED ON RELAY -- the fan-out lives in the console. And IMPORTING `internal/relay` BREAKS
  THE BUILD: `TestHandshakeHandlerIsNotWiredIntoAnyMux` (internal/relay/guards_test.go) fails if any package
  outside it imports it. Any topology view must be built from the traversed bus path in the audit log.
- `/v1/broadcast` ANSWERS 501 UNCONDITIONALLY -- telemetry must be DM, and nothing in this epic may wait on
  broadcast.
- COST WARNING (verbatim): every telemetry sample is two round trips (`/v1/mint` then `/v1/send`), a two-phase
  fsynced durable write, a permanent audit record that can never be deleted, and a sequence number that can
  never be reused. Ten nodes at one sample/second is ~864,000 permanent audit records a day whose entire content
  is "fine". This bus is engineered never to lose a message, which makes it a poor telemetry sink. Prefer the
  poll plane for anything the console can simply ask for.
- SEQUENCING RULING (operator-decided): the whole epic is filed at P2/P3 and MUST NOT START BEFORE `INVITE-GATE`
  LANDS. Enrolment is currently open, so this console would hold fleet-wide credentials against a bus anyone can
  enrol against.
- The console is a CONCENTRATION OF PRIVILEGE that does not exist today -- created by the request, not by the
  implementation. Mitigations belong in ADMIN-3 onward: distinct identity and IdentityDir per bus, 0600 storage,
  lease-bounded blast radius.

Reservations for this epic: epic key `ADMIN` = `epic-key` #3; task keys ADMIN-1..ADMIN-11 = `task-key-ADMIN`
#1..#11 (fresh namespace, DELIBERATELY UNSEEDED); control schema v1 = `admin-control-schema` #1. The
`-C` suffixed keys (ADMIN-C1..C3) are derived keys and need no reservation, but are unique.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
