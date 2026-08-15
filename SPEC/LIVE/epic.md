# EPIC LIVE — LIVE: Authenticated agent liveness and status subscriptions

[← all epics](../../SPEC.md)

**15 open / 15 total.** Full records live in `SPEC/LIVE/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (15)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| LIVE-1 | LIVE-1: Liveness contract and status state machine | todo | P0 | [task.md](LIVE-1--354e378c/task.md) | blocks [LIVE-10](LIVE-10--e06af8b7/task.md)<br>blocks [LIVE-11](LIVE-11--3662e698/task.md)<br>blocks [LIVE-12](LIVE-12--26b77c70/task.md)<br>blocks [LIVE-13](LIVE-13--f24219e0/task.md)<br>blocks [LIVE-14](LIVE-14--d4e8063c/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md)<br>+8 more (see task.md) | — |
| LIVE-11 | LIVE-11: Federation ownership, multi-hop liveness and partition semantics | todo | P0 | [task.md](LIVE-11--3662e698/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-3](LIVE-3--c5c0a210/task.md)<br>blocked by [RELAY-2](../RELAY/RELAY-2--654140d7/task.md)<br>blocks [LIVE-13](LIVE-13--f24219e0/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md) | [RELAY-2](../RELAY/RELAY-2--654140d7/task.md) [RELAY-19](../RELAY/RELAY-19--24e0bd11/task.md) |
| LIVE-15 | LIVE-15: Single- and multi-bus liveness subscription acceptance | todo | P0 | [task.md](LIVE-15--c9e65431/task.md) | blocked by [DEPLOY-3](../DEPLOY/DEPLOY-3--9eaf2d19/task.md)<br>blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-10](LIVE-10--e06af8b7/task.md)<br>blocked by [LIVE-11](LIVE-11--3662e698/task.md)<br>blocked by [LIVE-12](LIVE-12--26b77c70/task.md)<br>blocked by [LIVE-13](LIVE-13--f24219e0/task.md)<br>+8 more (see task.md) | [DEPLOY-3](../DEPLOY/DEPLOY-3--9eaf2d19/task.md) |
| LIVE-2 | LIVE-2: Monotonic timing, threshold boundary and clock-skew contract | todo | P0 | [task.md](LIVE-2--c0f4db11/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocks [LIVE-3](LIVE-3--c5c0a210/task.md)<br>blocks [LIVE-5](LIVE-5--7f62eeee/task.md)<br>blocks [LIVE-8](LIVE-8--742dd0ec/task.md) | — |
| LIVE-3 | LIVE-3: Authenticated heartbeat request/response and replay resistance | todo | P0 | [task.md](LIVE-3--c5c0a210/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-2](LIVE-2--c0f4db11/task.md)<br>blocks [LIVE-11](LIVE-11--3662e698/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md)<br>blocks [LIVE-4](LIVE-4--6376660b/task.md)<br>blocks [LIVE-5](LIVE-5--7f62eeee/task.md)<br>+3 more (see task.md) | — |
| LIVE-5 | LIVE-5: Durable last-observation and restart liveness reconstruction | todo | P0 | [task.md](LIVE-5--7f62eeee/task.md) | blocked by [AUTH-3](../AUTH/AUTH-3--d53e3b21/task.md)<br>blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-2](LIVE-2--c0f4db11/task.md)<br>blocked by [LIVE-3](LIVE-3--c5c0a210/task.md)<br>blocks [LIVE-10](LIVE-10--e06af8b7/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md)<br>+1 more (see task.md) | [AUTH-3](../AUTH/AUTH-3--d53e3b21/task.md) |
| LIVE-7 | LIVE-7: Liveness subscription authorization, privacy and anti-enumeration | todo | P0 | [task.md](LIVE-7--09bc72d0/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-3](LIVE-3--c5c0a210/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md)<br>blocks [LIVE-6](LIVE-6--5825cf57/task.md)<br>blocks [LIVE-8](LIVE-8--742dd0ec/task.md) | — |
| LIVE-8 | LIVE-8: Durable status-change notifications, cursors and idempotency | todo | P0 | [task.md](LIVE-8--742dd0ec/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-2](LIVE-2--c0f4db11/task.md)<br>blocked by [LIVE-3](LIVE-3--c5c0a210/task.md)<br>blocked by [LIVE-4](LIVE-4--6376660b/task.md)<br>blocked by [LIVE-5](LIVE-5--7f62eeee/task.md)<br>blocked by [LIVE-7](LIVE-7--09bc72d0/task.md)<br>+3 more (see task.md) | — |
| LIVE-10 | LIVE-10: Broadcast and roster-removal liveness interactions | todo | P1 | [task.md](LIVE-10--e06af8b7/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-5](LIVE-5--7f62eeee/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md) | — |
| LIVE-13 | LIVE-13: Liveness protocol compatibility and version negotiation | todo | P1 | [task.md](LIVE-13--f24219e0/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-11](LIVE-11--3662e698/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md) | — |
| LIVE-4 | LIVE-4: Heartbeat scheduler, jitter, backpressure and fanout limits | todo | P1 | [task.md](LIVE-4--6376660b/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-3](LIVE-3--c5c0a210/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md)<br>blocks [LIVE-8](LIVE-8--742dd0ec/task.md) | — |
| LIVE-6 | LIVE-6: Authorized status subscription HTTP API and CLI watch | todo | P1 | [task.md](LIVE-6--5825cf57/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-3](LIVE-3--c5c0a210/task.md)<br>blocked by [LIVE-7](LIVE-7--09bc72d0/task.md)<br>blocks [LIVE-14](LIVE-14--d4e8063c/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md)<br>relates to [TUI-4](../TUI/TUI-4--11898d9b/task.md) | — |
| LIVE-9 | LIVE-9: Unsubscribe, expiry and subscriber-resource cleanup | todo | P1 | [task.md](LIVE-9--8fa73253/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-8](LIVE-8--742dd0ec/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md) | — |
| LIVE-12 | LIVE-12: Liveness observability, metrics and audit | todo | P2 | [task.md](LIVE-12--26b77c70/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md) | — |
| LIVE-14 | LIVE-14: Document liveness and status-subscription contract | todo | P2 | [task.md](LIVE-14--d4e8063c/task.md) | blocked by [LIVE-1](LIVE-1--354e378c/task.md)<br>blocked by [LIVE-6](LIVE-6--5825cf57/task.md)<br>blocked by [LIVE-8](LIVE-8--742dd0ec/task.md)<br>blocks [LIVE-15](LIVE-15--c9e65431/task.md) | — |

## Closed tasks (0) — done, cancelled, superseded

_None._

## Epic description

Planning epic for authenticated agent liveness detection and authorized status subscriptions. Assumption: user “apic” means “epic.” Definition: an agent is alive iff an authenticated heartbeat response was observed strictly less than configurable threshold N seconds ago; authorized agents can subscribe to status changes. Creating this epic implements nothing.

Existing work is reused, not duplicated: AUTH-3 roster durability, POLL/CLI watch, RELAY federation, MTLS/invite/auth planes and WAL durability.

Open decisions / risks (LIVE-1 resolves before implementation): default/min/max N and validation; whether suspect grace/recovered state are needed; monotonic vs wall-clock behavior across restart; reuse of push/long-poll watch; partition semantics and ownership for foreign agents; notification guarantee, cursor retention and compaction; subscriber caps/fanout abuse and backpressure; flap/debounce/coalescing policy; privacy/status-reason disclosure; roster removal/revocation distinction; safe restart treatment of persisted observation.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
