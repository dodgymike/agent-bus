# EPIC INVMINT — INVMINT: mint invites without stopping the bus

[← all epics](../../SPEC.md)

**7 open / 7 total.** Full records live in `SPEC/INVMINT/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (7)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| INVMINT-1 | INVMINT-1: decide the invite-minting AUTHORITY MODEL and reconcile it with E4, FEDERATION… | todo | P2 | [task.md](INVMINT-1--1bed65a8/task.md) | _not fetched_ | [INVMINT-2](INVMINT-2--ef18b37a/task.md) [INVMINT-6](INVMINT-6--cedb8d6f/task.md) [INVMINT-7](INVMINT-7--174c7ba9/task.md) [INVMINT-3](INVMINT-3--8555e659/task.md) |
| INVMINT-6 | INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win… | todo | P2 | [task.md](INVMINT-6--cedb8d6f/task.md) | _not fetched_ | [INVMINT-1](INVMINT-1--1bed65a8/task.md) [INVMINT-7](INVMINT-7--174c7ba9/task.md) |
| INVMINT-7 | INVMINT-7: document the pre-minted-invite pool recipe, with the 0600 mode built in (quick… | todo | P2 | [task.md](INVMINT-7--174c7ba9/task.md) | _not fetched_ | [INVMINT-6](INVMINT-6--cedb8d6f/task.md) |
| INVMINT-2 | INVMINT-2: introduce an OPERATOR PRINCIPAL — a bus-scoped, non-agent identity that can au… | todo | P3 | [task.md](INVMINT-2--ef18b37a/task.md) | _not fetched_ | [INVMINT-1](INVMINT-1--1bed65a8/task.md) |
| INVMINT-3 | INVMINT-3: the invite-mint HTTP route on the running bus (NOT /v1/mint, which is message-… | todo | P3 | [task.md](INVMINT-3--8555e659/task.md) | _not fetched_ | [INVMINT-2](INVMINT-2--ef18b37a/task.md) [INVMINT-1](INVMINT-1--1bed65a8/task.md) [INVITE-GATE-ENFORCE](../INVITE/INVITE-GATE-ENFORCE--8297d7e2/task.md) [INVMINT-4](INVMINT-4--ea948fb0/task.md) |
| INVMINT-4 | INVMINT-4: the CLI subcommand for online invite minting + its AGENT_PROTOCOL.md entry (in… | todo | P3 | [task.md](INVMINT-4--ea948fb0/task.md) | _not fetched_ | [INVMINT-3](INVMINT-3--8555e659/task.md) [INVMINT-7](INVMINT-7--174c7ba9/task.md) |
| INVMINT-5 | INVMINT-5: invite REVOCATION and LISTING over the same online operator surface | todo | P3 | [task.md](INVMINT-5--18f15aa9/task.md) | _not fetched_ | [INVMINT-3](INVMINT-3--8555e659/task.md) [INVMINT-1](INVMINT-1--1bed65a8/task.md) |

## Closed tasks (0) — done, cancelled, superseded

_None._

## Epic description

Minting an invite currently requires STOPPING THE BUS. This epic is the follow-up that
`cmd/agent-bus/invite.go:26-28` already anticipates in a comment ("Minting while the bus runs needs a route or
an IPC channel and is deliberately NOT smuggled in here; it is filed as a separate follow-up rather than named
by a key invented in a comment"). This is that follow-up, with a reserved key.

## Status: OPERATIONAL GAP, NOT A DEFECT. NOTHING IS BROKEN OR INSECURE.

Filed at P2/P3 deliberately. A workaround exists, is documented below, and WORKS. Do not promote this on the
grounds that enrolment is blocked -- it is not.

## What is actually true today (verified on a throwaway rig, own port 19555, live bus untouched)

`INVITE-GATE-ENFORCE` (3cedcb7) made enrolment invite-only, closing a P0 in which ~4096 anonymous enrolments
permanently bricked the roster. That change was correct and authorised. Its operational consequence:
`agent-bus invite mint` appends an invite record through internal/wal's two-phase path, and the data directory
takes an EXCLUSIVE dirlock so two processes never append to one log. A RUNNING BUS HOLDS THAT LOCK, so mint
refuses with exitInviteBusRunning. Minting therefore requires a stop.

## THE PRE-MINTED POOL ALREADY WORKS. DO NOT "FIX" IT.

Read this before designing anything: THE STOP IS REQUIRED FOR MINTING, NOT FOR ENROLLING. Pre-minting a pool
of invites during ONE maintenance window lets agents enrol against a RUNNING bus indefinitely.

Measured end to end: primed a data dir, stopped the bus, minted 3 invites, started the bus, then enrolled
`worker-one` and `worker-two` against the RUNNING bus with ZERO restarts between them; a replay of the spent
invite was correctly refused (`invite not accepted`, collapsed with no which-sentinel oracle).
`cmd/agent-bus/invitepool_test.go` documents this same flow and calls it "the operator's real flow, not a
test-only shortcut".

A design in this epic that changes enrolment, or that treats the pool path as broken, has misread the problem.
The pool is the SUPPORTED WORKAROUND and remains supported after this epic ships.

## Motivation

Nine agents currently work on this bus. "Stop the bus to admit one agent" does not hold at that scale. Every
stop also invalidates every session -- sessions are in-memory only and do not survive a restart, so each agent
must re-run the handshake. NOTE PRECISELY: a restart costs a HANDSHAKE, not a RE-ENROLMENT. The roster is
durable. Anyone claiming a restart de-enrols agents is wrong, and a task written on that premise is wrong.

## The real fix

To mint with NO stop at all, THE RUNNING BUS MUST MINT, because it already holds the dirlock and owns the WAL.
There is currently no HTTP route for minting invites.

NAME COLLISION, STATED EXPLICITLY SO NOBODY TRIPS OVER IT: `/v1/mint` ALREADY EXISTS
(`internal/httpapi/messages.go:38 RouteMint`, `client/messages.go:27`) and it is MESSAGE-ID minting -- an
agent reserving a message id/sequence before `/v1/send`. It is entirely unrelated to invites. This epic MUST
NOT reuse that path, MUST NOT extend that handler, and any task that says "the mint route" must say WHICH.

## Constraints every design must satisfy (these are the epic's review conditions)

1. THIS IS A CHANGE TO THE AUTHORITY MODEL, NOT JUST A NEW ROUTE. `DECISIONS.md` E4 (2026-08-02) records "The
   first invite is minted server-side", and `cmd/agent-bus/invite.go:7-8` states the minting authority is
   FILESYSTEM ACCESS to the data directory -- the same model as wal-mac.key and the bus's private keys. Moving
   minting onto the network CHANGES WHO MAY MINT. That needs an explicit dated `DECISIONS.md` entry, and
   arguably operator consent, BEFORE ANY CODE. It is INVMINT-1 and it blocks everything else in the spine.
2. THIS EPIC REOPENS AN EXISTING OPERATOR RULING. The ADMIN epic (the local operator console) carries ruling
   D6: "NO ONLINE INVITE MINT -- `agent-bus invite mint` takes the exclusive dirlock and needs the bus
   stopped; the console links to the command instead." D6 was a SCOPING ruling for the console, not a
   judgement that an online mint is unsafe -- but it is an operator ruling and this epic contradicts it.
   INVMINT-1 must reconcile the two explicitly and record which supersedes which. Do not quietly ship a route
   that makes a live operator ruling false.
2b. AND THERE IS A THIRD PRIOR RULING, NOT JUST TWO. `DECISIONS.md` 2026-08-08 FEDERATION (e) applies the SAME
   E4 pattern to peer configuration: "peer configuration is offline under the dirlock, following the
   `invite mint` / E4 precedent", and it explicitly ACCEPTS the cost -- "online re-peering is given up; a
   topology change needs a restart" (`CONTRACTS-CLI.md:321-325`). So E4 is not a one-off; it is a PATTERN this
   codebase has now applied at least three times. INVMINT-1 must therefore state its SCOPE explicitly: is the
   decision invite-specific, or does it generalise to peer configuration and every future operator surface? A
   decision that silently reverses E4 for invites while leaving FEDERATION (e) standing leaves the codebase
   holding two contradictory rulings about the same question, which is worse than either answer.

2c. THE COUNTER-PRECEDENT WORTH KNOWING: `agent-bus healthcheck` is a server subcommand that deliberately
   TAKES NO LOCK AND WRITES NOTHING, and is therefore safe and expected to run against a RUNNING bus
   (`CONTRACTS-CLI.md:271-276`). It shows the design space is not binary. Note the contrast with
   `agent-bus peer list`, which writes nothing yet STILL takes the lock, because a read racing an append can
   see a half-written tail record. Minting WRITES, so neither of those gets it off the hook -- but any
   "just read it without the lock" shortcut proposed here must answer the `peer list` reasoning first.

3. THERE IS NO OPERATOR PRINCIPAL TODAY. Every route authenticates except enrolment, session begin/complete,
   `/healthz` and `/v1/info` -- but ALL of that authentication is AGENT authentication. An admin route needs a
   principal that is NOT an agent, or an agent credential would authorise minting the credentials that create
   agents. Invariant 11's mTLS is the natural place (an operator client certificate bound at init).
4. THE PEER PRECEDENT IS A PRECEDENT WITH A "DO NOT EXTEND" SIGN ON IT -- READ IT BEFORE CITING IT. The peer
   surface does authenticate by certificate alone, and security recorded that as a DELIBERATE NARROWING of
   invariants 3 and 11 at `internal/httpapi/authmw.go:339-351`. But the same comment block says, verbatim, "DO
   NOT EXTEND IT BY ADDING A PEER PATH TO unauthenticatedRoutes", warns "Do not read this arm as 'invariant 11
   is fully honoured on peer routes'", and states the narrowing REVERSES the moment a BUS-SCOPED bearer
   credential exists. So: reference it as the precedent that certificate-only authentication of a
   non-agent principal has been accepted here before -- do NOT reference it as licence to skip the
   invariant-11 cross-check. An operator principal that ends up with a token AND a certificate must cross-check
   them (a token presented over a certificate belonging to a different principal is REJECTED).
5. INVARIANT 1 MUST STAY STRUCTURAL, NOT A RULE. `agent-bus invite mint` has no `-invite-id` and no
   `-invite-secret` flag, AND `invite.MintRequest` (`internal/invite/store.go:273`) has NO FIELD for either.
   That ABSENCE is what makes a client-supplied id impossible rather than merely forbidden, and
   `TestInviteMintRejectsClientSuppliedSecret` (`internal/invite/mint_test.go:161`) pins it. A REQUEST BODY FOR
   A NETWORK MINT ROUTE IS EXACTLY WHERE THAT STRUCTURAL PROPERTY GETS LOST BY ACCIDENT -- a decoded struct
   with an extra field, or a passthrough into MintRequest, silently converts a structural guarantee into a
   validation rule. Every code task here carries this as a REVIEW CONDITION, and the route's request struct
   must be pinned by a test in the same way.
6. INVARIANT 7. Every capability ships with its `agent-bus`/`agent-busctl` subcommand and its
   `AGENT_PROTOCOL.md` entry IN THE SAME TASK. Nobody hand-writes HTTP; a capability with no subcommand is the
   missing half of the task, not a follow-up. `scripts/bus-*.sh` wrappers are RETIRED -- do not add one.
7. THE SECRET IS A BEARER CREDENTIAL IN AN HTTP RESPONSE. Today the invite secret's protection is a file mode,
   and the CLI enforces it (see INVMINT-7). Over the network it lands in a response body, and from there into
   a shell variable, a pipe, a CI log, a terminal scrollback. Whatever the design, response handling needs the
   SAME care the file mode gets today -- and it must not be logged, echoed, or written to a default-mode file
   by whatever consumes it.

## Two independent quick wins -- NOT blocked on the authority decision

INVMINT-6 (`-count N`) and INVMINT-7 (the documented 0600 pre-minting recipe) are self-contained, need no new
authority, no route and no decision, and can be done AT ANY TIME. They shrink the maintenance window and stop
every operator hitting the same wall on their first attempt. If only two things in this epic are ever done,
these are the two with the best ratio.

## Explicitly out of scope

Changing enrolment; changing the dirlock or allowing two writers to one log; any role or privilege tier inside
the bus beyond the single operator principal; relaxing the invite file mode check; reusing or extending
`/v1/mint` (message-id minting).

Reservations for this epic: epic key `INVMINT` = `epic-key` #5; task keys INVMINT-1..INVMINT-7 =
`task-key-INVMINT` #1..#7 (fresh namespace, deliberately unseeded -- no prior INVMINT task existed).
Filed by spec-keeper 2026-08-15 on operator instruction: FILE ONLY, do not start. The critical path is the
three-bus smoke test.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
