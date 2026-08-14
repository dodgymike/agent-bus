# Agent Bus — backlog index

> GENERATED MIRROR of the Spec Server (project slug `agent-bus`) — **never hand-edit**.
> Regenerate with `bash scripts/gen-spec-mirror.sh`. The server is the source of truth.
>
> This file lists EPICS ONLY. Task records live in the tree, one file each, with
> descriptions complete and untruncated:
>
> - `SPEC/<EPIC>/epic.md` — every task in the epic, open first, then closed
> - `SPEC/<EPIC>/<task>/task.md` — the full record for one task

**584 tasks in 28 epics — 386 open, 198 closed.**

| Epic | Open | Total | Summary | Tasks |
| --- | ---: | ---: | --- | --- |
| ACK | 12 | 12 | Planning epic for end-to-end message acknowledgement. Assumption: the user’s “ack/ack” means ACK/NACK (positi… | [SPEC/ACK/epic.md](SPEC/ACK/epic.md) |
| ADMIN | 14 | 14 | The local operator console for a set of agent-bus nodes. Ships as a NEW BINARY \`agent-busadm\` | [SPEC/ADMIN/epic.md](SPEC/ADMIN/epic.md) |
| AGENTIF | 4 | 13 | scripts/bus-*.sh wrappers (serve, enrol, agents, send, broadcast, wait, leave, peer) plus AGENT_PROTOCOL.md,… | [SPEC/AGENTIF/epic.md](SPEC/AGENTIF/epic.md) |
| ART | 18 | 18 | Planning epic for secure small inline and resumable chunked artifact transfer, including federated buses. Git… | [SPEC/ART/epic.md](SPEC/ART/epic.md) |
| AUTH | 16 | 23 | Agent submits a key, server signs it and returns a bearer token; token verification middleware; revocation/le… | [SPEC/AUTH/epic.md](SPEC/AUTH/epic.md) |
| CLI | 13 | 35 | A first-class command-line client for PEOPLE, distinct from the scripts/bus-*.sh agent wrappers. The wrappers… | [SPEC/CLI/epic.md](SPEC/CLI/epic.md) |
| COMMS | 13 | 13 | Inter-agent communication over the bus: establish by measurement, not assertion, what a well-formed message b… | [SPEC/COMMS/epic.md](SPEC/COMMS/epic.md) |
| CONTEXT | 23 | 25 | Cut the token cost of this repo's documentation without losing the rationale that stops agents breaking thing… | [SPEC/CONTEXT/epic.md](SPEC/CONTEXT/epic.md) |
| CORE | 4 | 22 | go.mod, cmd/agent-bus main, config/flags, and the two unauthenticated liveness/info routes. Foundation every… | [SPEC/CORE/epic.md](SPEC/CORE/epic.md) |
| CRYPTO | 10 | 12 | User ask (2026-08-02, verbatim): "Add to the backlog to add a mechanism to validate messages in the agent scr… | [SPEC/CRYPTO/epic.md](SPEC/CRYPTO/epic.md) |
| DEPLOY | 4 | 7 | agent-bus ships as a container and runs under Docker Compose (user instruction, 2026-08-02). Covers the Docke… | [SPEC/DEPLOY/epic.md](SPEC/DEPLOY/epic.md) |
| DOCS | 16 | 20 | README, DECISIONS.md seed, PROTOCOL.md (wire protocol + on-disk format), CONTRACTS.md. | [SPEC/DOCS/epic.md](SPEC/DOCS/epic.md) |
| DUR | 36 | 53 | Append-only WAL with length+checksum framing, the two-phase prepare-&gt;commit write path with fsync, recovery/r… | [SPEC/DUR/epic.md](SPEC/DUR/epic.md) |
| HANDOVER | 14 | 14 | A human -- first a maintainer, then an operator -- can read this repo, believe what it says, run it, and know… | [SPEC/HANDOVER/epic.md](SPEC/HANDOVER/epic.md) |
| ID | 11 | 19 | Bus id (persisted, stable across restarts), monotonic sequence allocator, agent id \`&lt;bus-id&gt;.&lt;name&gt;-&lt;n&gt;\`, mes… | [SPEC/ID/epic.md](SPEC/ID/epic.md) |
| IDEM | 19 | 32 | Every mutating operation carries a client-supplied idempotency key and is safe to retry, per invariant 10 (CL… | [SPEC/IDEM/epic.md](SPEC/IDEM/epic.md) |
| INVITE | 17 | 27 | Invites are single-use, expiring and revocable, minted by an operator; redeeming one is the ONLY route onto t… | [SPEC/INVITE/epic.md](SPEC/INVITE/epic.md) |
| LIVE | 15 | 15 | Planning epic for authenticated agent liveness detection and authorized status subscriptions. Assumption: use… | [SPEC/LIVE/epic.md](SPEC/LIVE/epic.md) |
| MSG | 1 | 6 | Agent list, broadcast, DM, message history, cursor semantics. | [SPEC/MSG/epic.md](SPEC/MSG/epic.md) |
| MTLS | 27 | 35 | Self-signed certificates, mutual TLS, no certificate authority anywhere; TLS is the required transport and th… | [SPEC/MTLS/epic.md](SPEC/MTLS/epic.md) |
| POLL | 0 | 3 | Park a waiter, wake on new message, timeout, cursor advance, no goroutine leak on client disconnect, thunderi… | [SPEC/POLL/epic.md](SPEC/POLL/epic.md) |
| PROCESS | 7 | 11 | Epic key reserved 2026-08-08 (reservation namespace epic-key, value 1). Work about HOW THIS PROJECT IS BUILT… | [SPEC/PROCESS/epic.md](SPEC/PROCESS/epic.md) |
| RATCHET | 2 | 8 | Research and de-risk the Signal-style double-ratchet requirement, with one governing constraint: WE DO NOT WR… | [SPEC/RATCHET/epic.md](SPEC/RATCHET/epic.md) |
| RELAY | 65 | 107 | Peer enrolment between buses, message relay, agent-list exchange, loop prevention via a traversed-bus path, p… | [SPEC/RELAY/epic.md](SPEC/RELAY/epic.md) |
| SIGN | 8 | 13 | RESCOPE, user instruction verbatim (2026-08-02): "ok, let's keep it simple and just use standard message auth… | [SPEC/SIGN/epic.md](SPEC/SIGN/epic.md) |
| TOOLING | 10 | 14 | Epic key reserved 2026-08-08 (reservation namespace epic-key, value 2). The tooling an agent in THIS repo can… | [SPEC/TOOLING/epic.md](SPEC/TOOLING/epic.md) |
| UNASSIGNED | 7 | 11 | — | [SPEC/UNASSIGNED/epic.md](SPEC/UNASSIGNED/epic.md) |
| ZZ-SCRATCH | 0 | 2 | — | [SPEC/ZZ-SCRATCH/epic.md](SPEC/ZZ-SCRATCH/epic.md) |

Directory names are `<key-or-title-slug>--<public-id-prefix>`; the public-id fragment is
what makes them unique, since `key` is null for a third of the backlog. Use the FULL
`public_id` (recorded in every `task.md`) for any Spec Server lookup — prefix resolution
does not exist server-side.

The tree shows task-to-task links in two clearly separated forms, and they are not the
same thing: **relations** are authoritative edges from the Spec Server (`blocks`, `supersedes`, `relates`, `follow_up` — fetched for this run), while
**referenced (derived)** links are merely key-shaped strings matched in description free
text. Derived links are best-effort and must not be read as a dependency list.
