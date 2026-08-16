# EPIC CONV — CONV: server-tracked multi-party conversations

[← all epics](../../SPEC.md)

**18 open / 18 total.** Full records live in `SPEC/CONV/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (18)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| CONV-PEERQUOTA | CONV-PEERQUOTA: bound conversation tracking per PEER on DISTINCT CONVERSATION IDS -- the… | todo | P1 | [task.md](CONV-PEERQUOTA--35cb7dc6/task.md) | _not fetched_ | [CONV-TRACK-ON-RECEIPT](CONV-TRACK-ON-RECEIPT--ed1e70ac/task.md) [RELAY-FU-IDEM-METER-BY-PEER](../RELAY/RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) [CONV-LIFECYCLE](CONV-LIFECYCLE--fe5d14d5/task.md) |
| CONV-AUTHZ-CREATOR | CONV-AUTHZ-CREATOR: only the creator may change the recipient list -- the arm that can sh… | todo | P2 | [task.md](CONV-AUTHZ-CREATOR--4abd8589/task.md) | _not fetched_ | [CONV-AUTHZ-ADMIN](CONV-AUTHZ-ADMIN--70dd573a/task.md) [CONV-NAME-INV6](CONV-NAME-INV6--a11d59cd/task.md) [CONV-SUCCESSION](CONV-SUCCESSION--422be55b/task.md) |
| CONV-CRASH | CONV-CRASH: crash-injection proof that conversation create + membership change recover to… | todo | P2 | [task.md](CONV-CRASH--3078ad4e/task.md) | _not fetched_ | [CONV-IDEM](CONV-IDEM--aae5f71e/task.md) [CONV-JOINPOINT](CONV-JOINPOINT--b18c8710/task.md) |
| CONV-CREATE-CLI | CONV-CREATE-CLI: mint a conversation -- HTTP route + agent-busctl subcommand + AGENT_PROT… | todo | P2 | [task.md](CONV-CREATE-CLI--627d20e0/task.md) | _not fetched_ | [SIGN-6](../SIGN/SIGN-6--c9e4aea1/task.md) [CONV-RECORD](CONV-RECORD--cd3524c2/task.md) |
| CONV-ID-SHAPE | CONV-ID-SHAPE: decide the conversation id shape -- bare UUID vs &lt;bus-id&gt;.&lt;conv-id&gt; (ATTRI… | todo | P2 | [task.md](CONV-ID-SHAPE--8914a5d8/task.md) | _not fetched_ | [CONV-RECORD](CONV-RECORD--cd3524c2/task.md) |
| CONV-IDEM | CONV-IDEM: conversation create + membership change idempotency -- three cases, NOT collap… | todo | P2 | [task.md](CONV-IDEM--aae5f71e/task.md) | _not fetched_ | [IDEM-FU-RESULTBYTES-VS-MAXRECIPIENTS](../IDEM/IDEM-FU-RESULTBYTES-VS-MAXRECIPIENTS--6a09349b/task.md) |
| CONV-JOINPOINT | CONV-JOINPOINT: NO BACKFILL -- define the join point as a durable POSITION, atomic with t… | todo | P2 | [task.md](CONV-JOINPOINT--b18c8710/task.md) | _not fetched_ | [CONV-CRASH](CONV-CRASH--3078ad4e/task.md) [CONV-MEMBER-CHANGE](CONV-MEMBER-CHANGE--03ebeed2/task.md) |
| CONV-LIFECYCLE | CONV-LIFECYCLE: decide whether a conversation ENDS, whether a participant may LEAVE, and… | todo | P2 | [task.md](CONV-LIFECYCLE--fe5d14d5/task.md) | _not fetched_ | [CONV-AUTHZ-CREATOR](CONV-AUTHZ-CREATOR--4abd8589/task.md) [CONV-PEERQUOTA](CONV-PEERQUOTA--35cb7dc6/task.md) [CONV-SUCCESSION](CONV-SUCCESSION--422be55b/task.md) |
| CONV-MEMBER-CHANGE | CONV-MEMBER-CHANGE: the change event -- a REMOVED participant gets exactly ONE final mess… | todo | P2 | [task.md](CONV-MEMBER-CHANGE--03ebeed2/task.md) | _not fetched_ | [CONV-JOINPOINT](CONV-JOINPOINT--b18c8710/task.md) [CONV-IDEM](CONV-IDEM--aae5f71e/task.md) [CONV-CRASH](CONV-CRASH--3078ad4e/task.md) [CONV-RECORD](CONV-RECORD--cd3524c2/task.md) [CONV-AUTHZ-CREATOR](CONV-AUTHZ-CREATOR--4abd8589/task.md) |
| CONV-MULTI-CLI | CONV-MULTI-CLI: expose multi-recipient send through the CLI -- COMMS-MULTI owns the handl… | todo | P2 | [task.md](CONV-MULTI-CLI--16686141/task.md) | _not fetched_ | [COMMS-MULTI](../COMMS/COMMS-MULTI--e7210d98/task.md) [COMMS-MULTI-DESIGN](../COMMS/COMMS-MULTI-DESIGN--8e56075b/task.md) |
| CONV-NAME-INV6 | CONV-NAME-INV6: is a user-supplied conversation NAME metadata, or a body wearing metadata… | todo | P2 | [task.md](CONV-NAME-INV6--a11d59cd/task.md) | _not fetched_ | [CONV-RECORD](CONV-RECORD--cd3524c2/task.md) |
| CONV-RECORD | CONV-RECORD: the durable conversation record -- wal.Entry.Kind "conversation" (reservatio… | todo | P2 | [task.md](CONV-RECORD--cd3524c2/task.md) | _not fetched_ | [CONV-ID-SHAPE](CONV-ID-SHAPE--8914a5d8/task.md) [CONV-NAME-INV6](CONV-NAME-INV6--a11d59cd/task.md) [CONV-MEMBER-CHANGE](CONV-MEMBER-CHANGE--03ebeed2/task.md) [CONV-CRASH](CONV-CRASH--3078ad4e/task.md) |
| CONV-SEND-BY-ID | CONV-SEND-BY-ID: address a conversation by id instead of tracking participants -- route +… | todo | P2 | [task.md](CONV-SEND-BY-ID--ce8bff7b/task.md) | _not fetched_ | [CONV-CREATE-CLI](CONV-CREATE-CLI--627d20e0/task.md) |
| CONV-SUCCESSION | CONV-SUCCESSION: creator-only mutation freezes a conversation when the creator's agent id… | todo | P2 | [task.md](CONV-SUCCESSION--422be55b/task.md) | _not fetched_ | [CONV-LIFECYCLE](CONV-LIFECYCLE--fe5d14d5/task.md) [INVMINT-2](../INVMINT/INVMINT-2--ef18b37a/task.md) [INVMINT-1](../INVMINT/INVMINT-1--1bed65a8/task.md) |
| CONV-TRACK-ON-RECEIPT | CONV-TRACK-ON-RECEIPT: a bus starts tracking a conversation on first receipt -- gated by… | todo | P2 | [task.md](CONV-TRACK-ON-RECEIPT--ed1e70ac/task.md) | _not fetched_ | [CONV-PEERQUOTA](CONV-PEERQUOTA--35cb7dc6/task.md) [RELAY-FU-IDEM-METER-BY-PEER](../RELAY/RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) [CONV-ID-SHAPE](CONV-ID-SHAPE--8914a5d8/task.md) |
| CONV-VS-THREAD | CONV-VS-THREAD: resolve the governance conflict with COMMS-THREAD-TRIAL/COMMS-THREAD-FIEL… | todo | P2 | [task.md](CONV-VS-THREAD--c31d1c40/task.md) | _not fetched_ | [COMMS-THREAD-TRIAL](../COMMS/COMMS-THREAD-TRIAL--3a7705b8/task.md) [COMMS-THREAD-FIELD](../COMMS/COMMS-THREAD-FIELD--35db4a7b/task.md) |
| CONV-AUTHZ-ADMIN | CONV-AUTHZ-ADMIN: the ADMIN arm of membership change -- BLOCKED, there is no admin princi… | blocked | P3 | [task.md](CONV-AUTHZ-ADMIN--70dd573a/task.md) | _not fetched_ | [INVMINT-2](../INVMINT/INVMINT-2--ef18b37a/task.md) [INVMINT-1](../INVMINT/INVMINT-1--1bed65a8/task.md) [CONV-SUCCESSION](CONV-SUCCESSION--422be55b/task.md) |
| CONV-DOCS | CONV-DOCS: conversations in PROTOCOL.md, CONTRACTS-* and AGENT_PROTOCOL.md -- the whole-e… | todo | P3 | [task.md](CONV-DOCS--0abb8f8d/task.md) | _not fetched_ | — |

## Closed tasks (0) — done, cancelled, superseded

_None._

## Epic description

Server-minted, server-tracked, multi-party CONVERSATIONS: a durable first-class object a client
creates through the CLI, addressed by a server-supplied id, carrying user-supplied naming metadata and
a mutable recipient list, tracked by every bus that sees it.

EPIC KEY RESERVED: `epic-key` namespace, value 8 (spec-keeper, 2026-08-15) -- claimed atomically so two
agents cannot mint the same key.

## Operator request, VERBATIM (2026-08-15)

> enable multiple recipients to a message - for example so that you could listen in on the conversation
> and respond to both if required. Also add user-supplied metadata that 'names' or tags the
> conversation, and server-supplied uuid. The conversation should be 'minted' and tracked on the local
> server with some useful metadata, including a uuid, routing information, etc. The client starts the
> conversation by using the client binary to ask the server to create the conversation, with
> receipients. When a server first receives a conversation message it should start tracking it so its
> clients can be aware and send using the uuid to send messages rather than track the other
> participants in the conversation. The receipeient list can change and a special change event message
> should be sent to all current receipients, including those that have been removed.

## SCHEDULING -- read before claiming anything here

Nothing in CONV is broken; this is a substantial NEW FEATURE. It sits BEHIND (a) the open P0
`RELAY-FU-IDEM-METER-BY-PEER` (in_progress, owned by codex) and (b) the RELAY federation epic's relay
egress critical path (owned by a feature-runner). Nothing in CONV is P0. The single P1
(CONV-PEERQUOTA) is P1 on SECURITY-BEFORE-SHIP grounds -- it must land in the same wave as the
implicit-tracking task it guards, not ahead of RELAY. Do not read P1 here as "ahead of RELAY P0s".

## MEASURED STATE OF THE CODE (verified 2026-08-15, do not re-derive)

- The DURABLE layer is already plural: `internal/store/message.go:211` `Recipients []string`;
  `:72` `const MaxRecipients = 64`; enforced at `:635` (handler path) and `:822` (DISK-decode path).
- The SIGNING layer is already plural: `internal/signing/canonical.go:65` `MaxRecipients = 4096`,
  sorted+deduplicated recipient list in the canonical layout. `internal/relay/signed_test.go:667`
  asserts `store.MaxRecipients <= signing.MaxRecipients`, so the two bounds are deliberate and the
  OPERATIVE durable bound is 64, NOT 4096.
- The RELAY INGEST path already accepts many recipients: `internal/hub/relayingest.go:283` bounds
  `req.Recipients` against `store.MaxRecipients`. So a multi-recipient message can arrive FROM A PEER
  today but cannot be ORIGINATED locally. That asymmetry is real and is the CONV-MULTI-CLI starting
  point.
- The narrowing to ONE recipient is on the REQUEST path only: `internal/httpapi/messages.go:196`
  `SendRequestBody.To string` (singular) and `internal/hub/hub.go` `SendRequest`. The RESPONSE and
  READ paths are ALREADY plural (`messages.go:741` `res.Recipients`, `:835` `toWireMessage` ->
  `m.Recipients`), and both already normalise nil to `[]` so the wire contract is "always an array".
- The CLI does NOT expose plurality: `cmd/agent-busctl/send.go:83-95` -- "send a direct message to one
  agent", "Delivers one message to one agent".
- NO conversation concept exists anywhere in `internal/`, `cmd/` or `client/`.
- `wal.Entry.Kind` is a STRING discriminator (`store.RecordKind = "message"`,
  `hub.SeqFloorRecordKind = "seqfloor"`), NOT a number. Neither consumed a numeric `record-type`; the
  `record-type` namespace holds exactly the four WAL FRAME types (Prepare/Commit/Abort/AuditMessage).

## RESERVATIONS ALREADY MADE FOR THIS EPIC

`wal-entry-kind` is a NEW reservation namespace created for this epic, because the STRING
`wal.Entry.Kind` discriminator was previously uncoordinated -- two parallel agents could both pick
`"conv"`. Seeded retrospectively (documented precedent: `cursor-format-version` v1 and
`store-record-version`):

| value | string | status |
| --- | --- | --- |
| 1 | `"message"` | retrospective seed, already shipped |
| 2 | `"seqfloor"` | retrospective seed, already shipped |
| 3 | `"conversation"` | RESERVED for CONV, unspent |
| 4 | `"convmember"` | RESERVED for CONV, unspent (membership-change record) |

NO numeric `record-type` was reserved, and reserving one would have been WRONG: by precedent a
business record rides inside the existing Prepare/Commit frames. If the CONV design turns out to need
a new WAL FRAME type, or a new standalone on-disk FILE, reserve from `record-type` /
`ondisk-format-version` AT THAT POINT -- never hand-pick. Same for `relay-wire-version` if conversation
state crosses a bus boundary on the wire.

## RELATIONSHIP TO COMMS (ruled EXTEND-vs-NEW, 2026-08-15) -- read this before filing anything here

Ruled NEW, but CONV does NOT own multi-recipient send. COMMS already owns it:
COMMS-MULTI-DESIGN (design) blocks COMMS-MULTI (implement `/v1/send` plurality + CLI + docs). CONV
must NOT duplicate those -- CONV-MULTI-CLI is wired `blocked by` COMMS-MULTI and exists only to prove
the agent-facing surface, not to re-implement the handler.

There is also a live GOVERNANCE TENSION with COMMS-THREAD-TRIAL / COMMS-THREAD-FIELD, which is
resolved by CONV-VS-THREAD and by nothing else. COMMS filed COMMS-THREAD-FIELD as `blocked` on purpose,
so that a wire-level `thread_id` could not be added before measurement showed convention was
insufficient. A server-tracked conversation object is STRICTLY HEAVIER than the wire field COMMS
refused to add un-measured. CONV therefore starts life in apparent conflict with a deliberate COMMS
decision, and that conflict is an OPERATOR-FACING question, not one an agent may quietly resolve.

## INVARIANTS THIS EPIC IS MEASURED AGAINST

- **1 + 2 (id authority + namespacing)**: the operator asked for a "uuid". A bare UUID is NOT
  namespaced, and every other id here is (`<bus-id>.<agent-id>`). See CONV-ID-SHAPE -- collision
  resistance is not the issue, ATTRIBUTION is.
- **6 (metadata and routing ONLY, never bodies)**: a user-supplied conversation NAME is arbitrary user
  content going into the append-only log. See CONV-NAME-INV6. This blocks the record format.
- **6 + 10 (unbounded peer-triggered state creation)**: "when a server first receives a conversation
  message it should start tracking it" is the SAME ATTACK SHAPE this repo forged end-to-end on
  2026-08-15 -- see CONV-PEERQUOTA and the `relates` edge to RELAY-FU-IDEM-METER-BY-PEER.
- **10 (idempotency, three cases not collapsed)**: CONV-IDEM.
- **4 + 5 (durable before ack, recover to a prefix)**: CONV-RECORD and CONV-CRASH; crash-injection
  tests are required evidence, "the code looks right" is not.
- **7 (the CLI is THE client)**: every capability ships its `cmd/agent-busctl` subcommand and its
  `AGENT_PROTOCOL.md` entry IN THE SAME TASK. `scripts/bus-*.sh` wrappers are RETIRED and adding one
  is forbidden. The client package cannot live under `internal/`.
- **11 (mTLS)**: cross-bus conversation tracking rides the existing peer transport; CONV adds no new
  trust path and must not.

## UNSPECIFIED BY THE OPERATOR -- raised as decision tasks, NOT invented

Who may change the recipient list (CONV-AUTHZ); does a newly-added participant see prior messages
(CONV-BACKFILL, a confidentiality question); can a participant LEAVE as distinct from being removed,
what happens when a participant's agent id disappears, and does a conversation END (CONV-LIFECYCLE,
which also bounds honest-use unbounded state).

## OPERATOR ANSWERS (2026-08-15) -- THESE ARE CONSTRAINTS, NOT OPEN DECISIONS

Operator, verbatim: "only the channel creator may change the receipient list, or an admin, and no
backfill"

### A. Authorization = CREATOR-OR-ADMIN. Split into two arms, and they do NOT ship together.

**THERE IS NO ADMIN PRINCIPAL IN THIS SYSTEM TODAY.** All authentication here is AGENT
authentication -- every route authenticates except enrolment, session begin/complete, `/healthz` and
`/v1/info`, and all of it resolves to an agent. This is the SECOND request to depend on a
non-agent operator identity; `INVMINT-2` (`ef18b37a-72b5-4b00-865f-edac288a0659`, "introduce an
OPERATOR PRINCIPAL -- a bus-scoped, non-agent identity") was filed for exactly this gap, most
naturally an operator client certificate under invariant 11's mTLS.

- The **creator arm** (CONV-AUTHZ-CREATOR) can ship WITHOUT an admin principal.
- The **admin arm** (CONV-AUTHZ-ADMIN) CANNOT, and is wired `blocked by` INVMINT-2 as a hard edge.
- "or an admin" MUST NOT become a silent TODO that ships as creator-only and quietly diverges from
  what the operator asked for. CONV-AUTHZ-CREATOR is required to fail CLOSED and say so.
- **Scheduling fact, stated so nobody is surprised:** INVMINT-2 is P3 and is itself `blocked by`
  INVMINT-1 (the invite-minting AUTHORITY MODEL decision). So the admin arm is behind a blocked P3.
- **AMBIGUITY, FLAGGED NOT RESOLVED:** "admin" may mean INVMINT-2's operator principal, or it may
  mean the ADMIN epic's `agent-busadm` console -- which is unbuilt, and whose D1 ruling is a browser
  console on loopback. These are different principals. CONV-AUTHZ-ADMIN must not pick one silently.

**The creator is recorded DURABLY in the conversation record.** Unlike the user-supplied NAME, the
creator is a fully-qualified agent id -- routing/identity metadata -- so it sits comfortably inside
invariant 6 and is NOT part of the CONV-NAME-INV6 question. Do not lump the two together.

**CREATOR-ONLY CREATES A SUCCESSION PROBLEM** -- raised as CONV-SUCCESSION. If only the creator may
change membership, a conversation whose creator's agent id disappears (never re-enrols, or its name
suffix advanced) is PERMANENTLY FROZEN: nobody may add or remove anyone, ever, and it is unbounded
retained state. The admin arm may be the intended escape hatch -- in which case the admin principal
becomes LOAD-BEARING rather than optional, which sharpens the INVMINT-2 dependency above.

### B. NO BACKFILL. A newly-added participant sees NOTHING sent before they joined.

**RATIONALE, recorded so nobody "improves" it later by adding history sync:** this removes the
retroactive-exposure hazard entirely. Adding someone to a conversation can never expose history they
were not party to. The rule is cheaper AND safer than any backfill design; a later history-sync
feature would reintroduce a confidentiality hazard the operator has already declined.

**The real work is defining the JOIN POINT precisely (CONV-JOINPOINT), and this repo has a specific
trap here.** Three notions have already caused two P0s by being conflated:

- `Seq` -- the pre-assigned, SIGNED IDENTITY. Not a position.
- `store.Message.Pos` -- the DELIVERY POSITION, stamped from `wal.Committed.CommitIndex`.
- `OriginMessageID` -- a CORRELATION key, INERT in every ordering/cursor/retention decision
  (landed `88c43b3`).

A join point is a POSITION question, so it is `Pos`, NEVER `Seq`.

**AND THERE IS A SECOND TRAP THAT THE NAIVE FIX WALKS STRAIGHT INTO** (measured 2026-08-15):
`store.Record` has NO `Pos` FIELD, deliberately. `internal/store/message.go:437` --
"Message.Pos is DELIBERATELY ABSENT, and adding it would be a mistake ... writing it INTO that entry
would record a fact the entry's own location already states -- and would create a second copy free
to disagree with the first." `Decode` returns `Pos == 0` and `Hub.Apply` stamps it from
`wal.Committed.CommitIndex`. So "make the join point durable" MUST NOT be implemented by adding a
`join_pos` field to the membership record -- that is the exact second-copy mistake that doc forbids.
The join point IS the commit index of the membership-change record itself, durable BY LOCATION.

Join and leave are OFF-BY-ONE HAZARDS IN OPPOSITE DIRECTIONS, and a naive membership check gets each
one wrong: **joining is EXCLUSIVE of prior history; leaving is INCLUSIVE of exactly one subsequent
message** (the final change event). Establishing the join point must be ATOMIC with the membership
change -- a gap or overlap either leaks one message backwards or silently drops one forwards. That
is a crash-injection test, not a code reading.

### C. What these answers CLOSED

CONV-AUTHZ and CONV-BACKFILL are NOT filed as open decision tasks; they are constraints above.
Everything else the operator did not specify -- id shape, the name-vs-invariant-6 question,
conversation lifecycle/end, participant LEAVE, and the COMMS threading conflict -- REMAINS OPEN and
belongs to the operator, not to us.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
