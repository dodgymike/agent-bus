# ACK-CONTRACT — the end-to-end delivery ACK/NACK contract

**Task:** `ACK-1` (public_id `e0ac42e1-5d63-422b-b163-ab10912d5ef4`), which **blocks all eleven other
ACK tasks** plus `ART-11`.
**Status:** DESIGN. No production code exists for anything below. This file is the specification the
implementing tasks are measured against; it is not a description of the running system.
**Invariants read in full before writing this:** 1, 2, 4, 5, 6, 10, 11 (`INVARIANTS.md`).

> **How to read the two kinds of statement in this file.**
> A sentence in the present tense with a `file:line` citation is a **fact about the code today** and
> was checked against the file, not remembered. A sentence with **MUST / MUST NOT / SHALL** is a
> **requirement on a future task** and describes nothing that exists. Where the two disagree, the
> code citation is the truth and the requirement is the work.

---

## 0. Which section each task implements

| Task | Sections that bind it |
| --- | --- |
| `ACK-2` durable local acceptance + lifecycle record | §3 (correlation key), §7 (durable record), §8 (state machine), §11 (retention) |
| `ACK-3` peer-hop wire semantics + correlation | §9 (wire shapes), §10 (**versioning — read before writing any frame**), §6.1–6.2 |
| `ACK-4` authorization / anti-forgery / privacy | §5 (closed NACK set), §6 (what "authenticated" means), §12 (no new disconnect), §13.3 (status oracle) |
| `ACK-5` multi-hop propagation | §9.4, §8.4 (hop receipt is not delivery) |
| `ACK-6` recipient acknowledgement boundary | §4 — **the ruling ACK-6 was blocked on** |
| `ACK-7` retry / idempotency / exactly-once terminal | §12 |
| `ACK-8` restart / crash-consistency | §14 (every clause the golden path defers) |
| `ACK-9` sender-visible status + CLI | §13 |
| `ACK-10` compatibility / version negotiation | §10 |
| `ACK-11` documentation | all of it |
| `ACK-12` three-bus acceptance | §15 |

---

## 1. The three planes, named once so nothing conflates them

The whole epic exists because three different facts have been collapsed into one word. They are
distinct events, at distinct times, attested by distinct parties, and **no one of them implies
another**.

| Plane | The fact it asserts | Who asserts it | Exists today? |
| --- | --- | --- | --- |
| **A. Local acceptance** | "This bus has committed and fsynced the message" (invariant 4) | The sender's own bus | **Yes.** `POST /v1/send` → **201** (`internal/httpapi/messages.go:747`). |
| **B. Peer-hop receipt** | "The next bus along took responsibility for a copy" | The adjacent peer bus, over mTLS | **Yes.** `RelayResponse{Accepted,Duplicate,MessageID}` (`internal/relay/message.go:202-220`), HTTP **200** (`internal/relay/relayhttp.go:391`). |
| **C. Recipient delivery ACK** | "The addressed agent's application received and accepted it" | The recipient agent | **No. Nothing in the tree does this.** |

**Correction to the brief, made from the code rather than from memory:** `POST /v1/send` returns
**201**, not 200, for both local and remote recipients (`internal/httpapi/messages.go:747`, and
`CONTRACTS-HTTP.md` "Cross-bus send"). The **200** that the operator ruled stays is
`POST /v1/peer/relay`'s ingest answer (`internal/relay/relayhttp.go:391`), which means "durable on
this bus, and nothing more". **Neither status code changes anywhere in this epic.** Plane C is
delivered as a *separate, asynchronous* fact, never by re-typing a status code.

### 1.1 The sentence this contract must never let anyone write

> "The send returned success, so the message was delivered."

Plane A is a durability statement about **one** bus. It is what invariant 4 promises and it is all
it promises. The narrowing of 2026-08-02 (`INVARIANTS.md` §4) is about damaged media and is **not**
licence to weaken plane A; it is likewise **not** an argument that plane C may be acknowledged before
it is durable. Every plane in this contract obeys invariant 4 independently: **an ACK or NACK is
reported to a sender only after the state transition that produced it is committed and fsynced.**

---

## 2. What is already built that this contract must sit on top of, not beside

Cited because a design that duplicates any of these is wrong by construction.

1. **A per-hop, durable, terminal state machine already exists.** `relay.OutboxState` is a closed
   enum `pending | delivered | abandoned`, terminal-absorbing, with `Terminal()` at
   `internal/relay/outbox.go:274-316`. An unrecognised wire spelling is an **error, never a default**
   (`parseOutboxState`, `outbox.go:316-330`) — the posture this contract copies for every new enum.
2. **The per-hop record already carries the correlation key.** `OutboxRecord{JobID, PeerBusID,
   OriginMessageID, Size, ContentSHA256, EnqueuedAt, State, SettledAt, Reason}`
   (`internal/relay/outbox.go:388-441`), written by `Forwarder.Enqueue` at
   `internal/relay/forward.go:886-891`. `JobID = DeriveJobID(PeerBusID, OriginMessageID)`.
3. **`Reason` is REQUIRED on `abandoned` and forbidden elsewhere** (`outbox.go:435-441`) — invariant
   6's "the defect is the SILENT discard, not the discard".
4. **The durable write path is `wal.Entry{Kind, Body, Idem, Audit}` through `durable.Write`**, with
   the applied-key record riding in the **same prepare payload as the effect, one fsync**
   (`internal/wal/log.go:98-113`). Appliers are registered in one map at
   `cmd/agent-bus/main.go:554`; `relay.OutboxRecordKind = "outbox"` (`outbox.go:93`) is registered at
   `cmd/agent-bus/main.go:624`, and the log is opened with `Applier:` and **never** `Checkpoints:`
   (`cmd/agent-bus/main.go:630`). **Checkpoints are therefore irrelevant to invariant 4 here and no
   part of this contract may be designed around them.**
5. **A closed error-code vocabulary already exists on the peer surface** —
   `internal/relay/handshake.go:22-76` (`CodeInvalidRelay`, `CodeIdempotencyViolation`,
   `CodeUnsigned`, `CodeBadSignature`, `CodeUnpeeredBus`, `CodeUnknownRecipient`, …). §5 extends this
   vocabulary; it does not start a second one.
6. **A derived, non-round retention window with a fail-closed cap already exists** —
   `internal/idem/retention.go` (`PeerOutageBudget = 24h` at `:27`, `RetentionWindow` at `:78`,
   `MaxEntries = 65536` at `:152`, per-agent fair share at `:228-241`). §11 derives the ACK window the
   same way and adopts the same eviction posture.

---

## 3. DECISION 1 — the correlation key

> **RULING: the correlation key is the ORIGIN bus's server-minted message id, reached through
> `store.Message.OriginID()`. It is SERVER-MINTED (by the origin server), it is NOT sender-supplied,
> and it is NOT a fourth identifier — it is the existing third one.**

`internal/store/message.go:167-194` already names it and already forbids the drift the brief warned
about, in the file itself:

```
//	Seq                IDENTITY. Server-minted, client-signed, spendable out of order.
//	Pos                DELIVERY POSITION. The WAL commit index; what cursors point at.
//	OriginMessageID    CORRELATION KEY. It answers "which message on the ORIGIN bus is this a
//	                   local copy of", and NOTHING else.
//
// IT TAKES PART IN NO ORDERING, NO CURSOR AND NO RETENTION DECISION.
```

`OriginID()` (`internal/store/message.go:293-297`) is **the one place** the "origin id when set,
local id otherwise" rule is written down; the doc comment above it forbids re-spelling that branch at
a call site. The relay layer already uses exactly this value as the wire idempotency key
(`PROTOCOL.md` §10: "the relay idempotency key … **is** the origin's `message_id`", enforced by
`ValidateRelayRequest`) and as `OutboxRecord.OriginMessageID` (`outbox.go:404-411`).

**Why not a fourth identifier.** `Seq`/`Pos`/`OriginMessageID` have already produced three defects
here. A new ACK-only id would be a fourth axis on the same object with no fact of its own to carry:
it would have to be minted by *some* bus, and whichever bus minted it, every other bus would have to
learn the mapping — which is precisely the job `OriginMessageID` already does.

**Why it cannot drift.** It is `<origin-bus-id>-<seq>` (`ids.MessageID`), so it is bus-namespaced
(invariants 1 and 2) and globally unambiguous without any registry. It is **inert**: nothing orders,
cursors or expires on it, so no future ordering change can move it.

### 3.1 Collisions

| Case | Ruling |
| --- | --- |
| Two messages on **one** bus | Impossible. Ids are never reused, including across restarts (invariant 1), and the durable seq floor enforces it (`internal/hub/seqfloorfile.go`). |
| Two messages on **different** buses | Impossible by namespacing — the bus half differs (invariant 2). |
| A peer **claims** a correlation key that names a third bus | **This is the real case and it is not a collision, it is a forgery attempt.** Ruled in §6.2: an ACK/NACK frame is authoritative only for a key that binds to an obligation this bus itself wrote. |
| A local sender supplies a correlation key on the ACK-status API | It is **input to be validated, never an identity to be trusted** (invariant 1). Unknown or not-yours → §13.3's uniform answer. |

### 3.2 The per-recipient sub-key, decided now so broadcast never needs a schema change

The ACK state record is keyed on **(correlation key, recipient agent id)**, never on the correlation
key alone. A directed message with N recipients produces N rows. This costs nothing today
(`store.MaxRecipients` is 64) and is the single decision that lets §5.5's broadcast ruling be
revisited later without touching the on-disk record.

---

## 4. DECISION 4 — the recipient acknowledgement boundary (`ACK-6` unblocks here)

> **RULING: delivery to an inbox or a poll is NOT recipient receipt. An EXPLICIT application ACK is
> required. Plane C is reached only by the recipient calling the new ACK route; it is never inferred
> from a cursor advancing.**

Three independent reasons, strongest first.

1. **The bus cannot know what the recipient knows.** The bus carries the sender's signature as
   opaque bytes and **never verifies it** — "the BUS enforces SHAPE (present, exactly
   `signing.SignatureSize` bytes) and the RECIPIENT enforces AUTHENTICITY"
   (`internal/store/message.go:260-270`, restated at `internal/httpapi/messages.go:238-243`). A bus
   that auto-ACKed on poll would be asserting, on the recipient's behalf, a fact only the recipient
   can establish. That is the same class of error as the bus vouching for a signature, and §6.3
   forbids it there too.
2. **There is no server-side per-recipient delivery state to derive it from, and adding one is
   strictly more state than an explicit ACK.** The cursor is an **opaque, client-held, agent-bound
   delivery position** (`internal/hub/cursor.go:39-60`; the store that persists it is on the client,
   `client/cursorstore.go`). Inferring receipt would require a new per-(agent, message) server table —
   more state than the ACK record §7 defines, and less truthful.
3. **A poll is replayable, so an inferred ACK would fire repeatedly for one message.** Delivery is
   at-least-once and an unrecognised cursor version is **accepted and remapped to position 0**, i.e.
   one full replay of the retained window (`CONTRACTS-HTTP.md`, "The cursor"). One message would
   produce many "receipts".

**Consequences that must be implemented, not assumed:**

- A message that has been polled but not ACKed remains sender-visible as `accepted` / `in_flight`.
  **There is no `polled` state and one MUST NOT be added** — it would require exactly the table
  reason 2 refuses.
- The recipient ACK route is a **mutating operation** and therefore carries an idempotency key and is
  durable across restart (invariant 10). Re-ACKing the same (key, recipient) with the same outcome is
  a legitimate retry: return the original result, re-apply nothing, do not disconnect.
- An ACK for a message the recipient was never addressed in is refused with the §13.3 uniform answer
  (it is a probe otherwise).
- **Honest cost, stated rather than hidden:** an agent that never ACKs leaves its sender's state
  non-terminal until §11's window expires. That is correct — the sender genuinely does not know — and
  it is why §13 requires `unknown` to be a first-class, documented answer rather than an error.

---

## 5. DECISION 3 — what a NACK may carry: the CLOSED class set

> **RULING: a NACK carries a CLASS from the closed set below and NOTHING ELSE that any party other
> than the emitting bus's own code chose. There is no free-text reason field on any ACK/NACK wire
> frame, in any durable ACK record, or in any sender-visible status response.**

### 5.1 The invariant-6 rule, stated as a rule

The audit log records **metadata and routing only, never bodies**
(`INVARIANTS.md` §6; enforced structurally — `wal.AuditRecord` has no body field,
`internal/wal/audit.go:145-200`). A reason string sourced from a recipient or from a payload is a
body by another name: "delivery failed because `<recipient text>`" puts recipient-chosen bytes into a
durable, append-only, un-rewritable trail. So:

- **A NACK class is a compile-time constant chosen from the enum below by the code of the bus that
  emits it.** It is never assembled, never templated, never concatenated.
- **`OutboxRecord.Reason` does NOT go on the wire and is NOT returned to a sender.** It is a
  bus-authored, bounded, sanitised local string (`internal/relay/outbox.go:171-190` — bounded
  precisely because "a peer's error code can reach it") that exists for the operator log and the
  local durable record. Mapping it onto a sender-visible field would re-export a peer-influenced
  string to a third party. The mapping is one-way: **class → record; never record.Reason → wire.**
- A frame carrying an **unrecognised class is REJECTED, never defaulted** — the posture
  `parseOutboxState` already takes (`outbox.go:316-330`), and for the same reason: guessing turns a
  corrupt or future-format frame into a plausible-looking outcome.

### 5.2 The set (CLOSED — exactly twelve)

**Bus-emitted** — asserted by the sender's own bus or by a hop, about routing:

| Class | Meaning | Emitted where |
| --- | --- | --- |
| `no_route` | No configured peer for the destination bus half. | `relay.Registry.Route` miss (`forward.go:860-865` logs the equivalent today). |
| `no_such_recipient` | The destination bus has no such agent. | Existing `CodeUnknownRecipient` (`handshake.go:76`, RELAY-21). |
| `hop_refused` | The next hop answered finally and negatively. | Existing final codes only: `CodeUnsigned`, `CodeBadSignature`, `CodeUnpeeredBus`, `CodeInvalidRelay`, `CodeInvalidBusPath` (`handshake.go:39-68`). |
| `hop_unauthenticated` | The peer could not be authenticated as a principal. | `RequirePeerPrincipal` refusal (`internal/httpapi/peermount.go:316-363`). |
| `loop_dropped` | Already traversed / split horizon. | Existing `DropLoop` (`internal/relay/message.go:42`). |
| `fanout_exceeded` | Over `maxOnwardBusesPerMessage` (8). | `cmd/agent-bus/relaywiring.go`. |
| `horizon_expired` | The retry horizon ran out; the outbox settled `abandoned`. | `OutboxAbandoned` (`outbox.go:286-291`). |
| `local_capacity` | A local durable resource refused the work, fail-closed. | Outbox capacity / `idem.ErrCapacity` / `ErrAgentQuota`. |
| `obligation_lost` | A durably-accepted onward obligation was abandoned at restart. | **RELAY-48.** Cannot occur on the golden path; detection **deferred to ACK-8** (§14). |

**Recipient-emitted** — exactly three, chosen by the recipient application from this enum:

| Class | Meaning |
| --- | --- |
| `recipient_refused_policy` | The application declines it. |
| `recipient_refused_undecodable` | The application could not decode/verify it. |
| `recipient_refused_not_addressed` | The application does not consider itself the addressee. |

**THE SINGLE HOME OF THESE SPELLINGS IS `internal/ack` (ACK-13, 2026-08-16).** The twelve classes,
the two attestation labels (§6.3) and the five states (§8.1) are declared in `internal/ack` and
nowhere else inside the server: `internal/relay` consumes them through Go **type aliases**
(`relay.AckClass = ack.Class`, `relay.AckOutcome = ack.State`,
`relay.AckAttestation = ack.Attestation`) and declares no spelling of its own, which
`TestAckVocabularyHasOneHome` (`internal/ack/vocabulary_test.go`) enforces by walking relay's syntax
tree. They were declared TWICE until ACK-13 — as strings in `internal/ack` and as `uint8` enums in
`internal/relay` — because ACK-2 and ACK-4 were run in parallel; a closed enum that exists twice is
not closed, and the failure mode is silent (one side gains a thirteenth member, the other refuses it
as a protocol violation rather than as version skew). Two copies remain OUTSIDE the server and are
deliberate: `client/ack.go`, because `client/` may not import `internal/` (invariant 7), and
`internal/signing`, whose constants are a FROZEN WIRE ALPHABET pinned against `internal/ack` by
`internal/signing/ackvocab_external_test.go`.

**Why these three and no more.** Each is a fixed constant that reveals *that* something failed, never
*what*. `recipient_refused_undecodable` in particular says "decoding failed" and says nothing about
the bytes that failed to decode — which is the exact line invariant 6 draws. Any request for a
richer explanation is a request for a body in the log and MUST be refused; the recipient and sender
already have an end-to-end message channel for prose, and it is the right place for it.

### 5.3 What a NACK frame carries in total

`{ wire_version, correlation_key, recipient, class, terminal:true, emitted_at, attestation }` — and
that is the whole frame (§9.2). Every field is either a server-minted id, a closed enum, a timestamp,
or a signature. **There is no field whose length a remote party chooses.**

### 5.4 Positive ACKs carry no class at all

A `delivered` outcome has nothing to explain. Adding an optional class to it would create a channel
where none is needed.

### 5.5 DECISION 5 — broadcast aggregation

> **RULING: broadcast is OUT OF SCOPE for this contract while `POST /v1/broadcast` answers 501. No
> aggregation policy is defined, because defining one would settle `SIGN-3` by accident.**

The evidence that this is the cheapest *correct* answer and not a punt:

- `POST /v1/broadcast` answers **501 after authentication and before the body is decoded**
  (`internal/httpapi/messages.go:452-467`).
- **Relayed broadcasts are refused on ingest** with `ErrUnsignable`, so a peer cannot introduce one
  either (`internal/relay/message.go` / `ValidateRelayRequest`; `DECISIONS.md` 2026-08-08 (c)).
- `auditContentHash` **fails closed** on a broadcast, and its comment states exactly why answering
  here would be wrong: "Substituting the bare-body hash, or a digest over a synthesised audience,
  would settle SIGN-3 by accident — in a file nobody would think to read when they came to settle it
  properly" (`internal/hub/audit.go`, `auditContentHash`).
- A broadcast has **no canonical audience** under signing format v1: `signing.Canonicalize` rejects
  an empty recipient set and `store.Message` stores a broadcast as a **flag**, deliberately not an
  expanded roster (`internal/store/message.go`, `Broadcast` field; `DECISIONS.md` Decision 4).

An ACK correlates a **(message, recipient)** pair. A broadcast has no recorded recipient set, so
there is no pair to key on and nothing to aggregate over. Answering anyway would mean inventing an
audience — the same mistake, in a document eleven tasks build on.

**But two constraints are fixed now, so this is a decision and not a deferral:**

1. **The ACK record is keyed on (correlation key, recipient) from day one** (§3.2). When SIGN-3
   defines a canonical audience, a broadcast produces N rows through the existing schema. **No
   on-disk change, no new record type, no migration.**
2. **When aggregation is eventually specified it MUST be per-recipient with NO roll-up and NO
   quorum.** An aggregate ("3 of 5 delivered") is a **roster-size oracle**: it discloses bus
   membership to any sender, and the broadcast flag exists precisely so the roster is *not* frozen
   into the record (`internal/store/message.go`, `Broadcast`). A sender may learn the fate of
   messages to recipients it named; a broadcast names none.

---

## 6. DECISION — what "authenticated ACK/NACK" can mean, given that the bus verifies no signatures

The brief's ground truth is confirmed in code: the bus carries the signature and **never verifies
it** (`internal/store/message.go:260-270`), enforces shape only
(`internal/httpapi/messages.go:238-243`), and **no endpoint distributes messaging public keys** — a
repository-wide grep finds no such route. So "authenticated" is defined in three layers, and the
contract requires that **the status API always says which layer it has**.

### 6.1 Layer 1 — bus attribution (REAL TODAY)

A peer-hop ACK/NACK arrives on the peer surface, which is gated by `RequirePeerPrincipal`: it is
fail-closed, refuses when no resolver is configured, when there is no TLS, when no certificate was
presented, and when the leaf resolves to no single adjacent bus
(`internal/httpapi/peermount.go:66-73`, `:316-363`). This authenticates **which bus** sent the frame.

Invariant 11's cross-check clause is satisfied on this surface by a documented **narrowing**, not by
compliance: `peermount.go:85-92` states plainly that invariant 11 asks for two factors cross-checked
and **one** authorises here — the certificate — because a peer handler never sees an agent principal
at all and there is no pair to cross-check. **This contract inherits that narrowing and MUST NOT
widen it.** In particular: an ACK frame MUST NOT be accepted on the agent surface on behalf of a peer
bus, and an agent session token MUST NOT be consulted on the peer surface.

### 6.2 Layer 2 — obligation binding (REAL TODAY, costs no new state)

> **RULE: a peer-hop ACK/NACK from peer `P` is authoritative for correlation key `K` and recipient
> `R` if and only if `DeriveJobID(P, K)` names an outbox job THIS bus durably wrote.**

This is the anti-forgery core of `ACK-4`, and it is computable entirely from
`internal/relay/outbox.go`'s existing durable record — no new index. It closes, without a new
mechanism:

- **reflection** — a peer settling an obligation it was never given;
- **cross-route forgery** — a peer settling the copy that went via a *different* peer (the job id is
  keyed on the peer, `outbox.go:390-396`);
- **third-party settlement** — a peer settling a key whose bus half names some other bus, because we
  never wrote a job to *that* peer for it.

Note carefully what layer 2 does **not** try to do: in an A→B→C chain, C's ACK reaches A via B, and
the bus half of `K` is A's, not C's. A "the bus half must equal the ACKing peer" rule would be wrong
and would break multi-hop. The job-id binding is the correct test at every hop.

### 6.3 Layer 3 — end-to-end recipient attestation (SHAPE ONLY, and it must be LABELLED as such)

A recipient ACK/NACK carries a detached Ed25519 signature over canonical ACK bytes.

- **Every bus treats it as opaque bytes and checks SHAPE ONLY** — present, exactly
  `signing.SignatureSize` (64) bytes — byte-for-byte the posture already taken for message
  signatures. No bus verifies it, and no bus may claim to.
- **The canonical ACK bytes MUST be produced by `internal/signing`, reusing §8.2's existing
  length-prefixed framing** (fixed field order, big-endian fixed-width integers, every
  variable-length field length-prefixed). **Invariant 9 binds here:** this adds an ENCODING, never a
  primitive. Specifically forbidden: a new MAC, a new KDF, a bespoke construction from good
  primitives, and any reuse of the WAL MAC key (`wal-mac.key`) for anything on the wire.
- **The status API MUST label attestation, never imply it.** §13.2's `attested_by` field takes
  exactly `peer_bus` or `recipient_signature_unverified`. There is deliberately no value meaning
  "verified", because nothing can produce one.

> **OPEN QUESTION, flagged rather than guessed (§16, Q1):** no endpoint distributes agents' messaging
> public keys, so a *sender* cannot verify layer 3 either. Until that exists, a recipient NACK is
> **attributable to a bus, and end-to-end unverifiable by anyone**. That is a real limitation of this
> epic and `ACK-11` MUST document it in those words.

---

## 7. The durable ACK record (`ACK-2`)

### 7.1 Where it lives

A new WAL entry kind, written through the **same** `durable.Write(wal.Entry{...})` path that
`internal/hub/hub.go:1798` and `internal/relay/outbox.go:1354` already use, and registered as a new
applier in the one map at `cmd/agent-bus/main.go:554`.

- **`Kind` MUST be a new application discriminator string** (proposed `"ack"`). `wal.Entry.Kind` is a
  free-form application string and **is not** a reserved `record-type` NUMBER
  (`CONTRACTS-ONDISK.md:825-827`), so this needs no `record-type` reservation.
- **If — and only if — a new numbered WAL frame type or a new on-disk FILE format is introduced, its
  number MUST be RESERVED from the Spec Server (`record-type`, currently max **4**;
  `ondisk-format-version`, currently max **7**), never picked by eyeballing the constant.** This
  design needs neither; a task that finds it does must reserve first.
- **The applied-key record rides in the SAME `wal.Entry.Idem` payload as the effect** so the ACK
  transition and its idempotency memory commit in one fsync (`internal/wal/log.go:98-113`). A second,
  separately ordered write would leave the window invariant 10 exists to close.
- **Checkpoints are not involved.** The log is opened with `Applier:` only
  (`cmd/agent-bus/main.go:630`); durability is `Begin(fsync) → Commit(fsync)`
  (`internal/wal/log.go:799-815`). No clause below may depend on a checkpoint.

### 7.2 The record

| Field | Type | Notes |
| --- | --- | --- |
| `correlation_key` | string | §3. `<origin-bus>-<seq>`, ≤ `ids.MaxMessageIDLen` (85, `internal/ids/messageid.go:15`). |
| `recipient` | string | Fully-qualified (invariant 2), ≤ `ids.MaxAgentIDLen` (`internal/ids/agentid.go:38`). |
| `sender` | string | The authenticated principal that sent the message. Authorises §13.3. |
| `state` | enum | §8. Fixed **string** on disk, never a number — `OutboxState.String()`'s reasoning (`outbox.go:297-306`): a numeric enum in a durable record is unreadable to an operator and silently changes meaning if the constants are reordered. |
| `class` | enum | §5.2. Set **iff** `state` is a negative terminal; forbidden otherwise, validated in both directions (the `OutboxRecord.Reason` rule, `outbox.go:435-441`). |
| `attested_by` | enum | §6.3. `peer_bus` \| `recipient_signature_unverified`. |
| `accepted_at` | time | This bus's clock, as `store.Message.SentAt` is (`message.go:218-219`). |
| `settled_at` | time | Set **iff** terminal. It is the input to §11's sweep, so a terminal record without one could never be swept. |

**There is no variable-length free-text field, by construction (§5.1).** The worst-case footprint is
therefore fixed and derivable exactly as `idem.MaxRecordBytes` is
(`internal/idem/retention.go:81-112`), which is what makes §11's cap a bound rather than a hope.

**The record MUST NOT carry:** the body, any hash of the body (the audit trail already holds the
content hash; duplicating it here creates two fields that must agree), `Seq`, or `Pos`. It carries no
ordering axis at all — §3's "inert" property is the whole reason the correlation key is safe.

---

## 8. DECISION 2 — the state machine

### 8.1 The states

**Five durable states. Three are terminal and terminal is ABSORBING: a terminal state is NEVER
revisited, never reopened, and never downgraded to a non-terminal one.**

| State | Terminal | Meaning |
| --- | --- | --- |
| `accepted` | no | Committed and fsynced on the sender's bus (plane A). |
| `in_flight` | no | At least one hop is owed: a `pending` outbox job exists. Remote recipients only. |
| `delivered` | **YES** (positive) | The recipient application ACKed (plane C, §4). |
| `refused` | **YES** (negative) | An authenticated terminal NACK arrived. Carries a `recipient_*` class. |
| `undeliverable` | **YES** (negative) | This bus will never deliver it. Carries a bus-emitted class. |

These five are declared once, in `internal/ack` (§5.2's note on the single home): `relay.AckOutcome`
is a type alias for `ack.State`, so the three terminal spellings a frame may carry and the durable
ones are the same strings by construction. The two NON-terminal states are representable in that
alias and must never travel on a frame — `!outcome.Terminal()` is what refuses them
(`internal/relay/ack.go`).

**`unknown` is a REPORTING value, not a state.** It is what §13 returns when no record is retained
(swept, never created, or not yours). It is **never written to the durable record** — writing
"I don't know" durably is how a real terminal outcome gets overwritten by ignorance.

### 8.2 State × event → next state

Events: **E1** local durable commit · **E2** outbox job enqueued `pending` · **E3** hop ACK
(`RelayResponse.Accepted`) · **E4** hop settled `abandoned` / final hop NACK · **E5** recipient ACK ·
**E6** recipient NACK · **E7** duplicate frame, same outcome · **E8** conflicting frame, different
terminal outcome · **E9** retention sweep.

| State \ Event | E1 | E2 | E3 | E4 | E5 | E6 | E7 | E8 | E9 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| *(none)* | `accepted` | — | reject¹ | reject¹ | reject¹ | reject¹ | — | — | — |
| `accepted` | replay² | `in_flight` | **`accepted`**³ | `undeliverable` | `delivered` | `refused` | replay² | violation⁴ | swept |
| `in_flight` | replay² | `in_flight`⁵ | **`in_flight`**³ | `undeliverable` | `delivered` | `refused` | replay² | violation⁴ | swept |
| `delivered` | replay² | ignore⁶ | ignore⁶ | ignore⁶ | replay² | violation⁴ | replay² | violation⁴ | swept |
| `refused` | replay² | ignore⁶ | ignore⁶ | ignore⁶ | violation⁴ | replay² | replay² | violation⁴ | swept |
| `undeliverable` | replay² | ignore⁶ | ignore⁶ | replay² | violation⁴ | violation⁴ | replay² | violation⁴ | swept |

1. **reject** — no record exists for this (key, recipient), so nothing binds the frame (§6.2). Refuse
   with the §13.3 uniform answer, log it, **do not disconnect** (§12).
2. **replay** — invariant 10, first case: same key + same payload is a legitimate retry. Return the
   ORIGINAL result, re-apply nothing, do not error, **do not disconnect**.
3. **A HOP ACK DOES NOT ADVANCE THE SENDER-VISIBLE STATE. This is the point of the epic.** A peer
   taking responsibility for a copy is plane B; the sender's state is about plane C. Recording it
   changes the *hop* record (`OutboxDelivered`, which already exists) and nothing else.
4. **violation** — invariant 10, second case: same key, different payload/outcome. **Reject and log
   it, but do NOT disconnect** (narrowed 2026-08-08 by user decision). This is the monotonicity rule
   the outbox already enforces (`outbox.go:249`, `:1685`, `:1702`): terminal never goes back to
   pending, and a *different* terminal never overwrites an existing one.
5. `in_flight` → `in_flight` on a second destination: a message with recipients on two peer buses has
   several hops and one sender-visible row per recipient (§3.2).
6. **ignore** — terminal is absorbing. Counted and logged; not an error, because a late hop
   settlement after a recipient already ACKed is *normal* under at-least-once delivery.

### 8.3 The local-recipient path

A recipient on this bus never reaches `in_flight`: `accepted` → (`delivered` | `refused`). This is
the smallest end-to-end path and is what `ACK-6`'s narrowest test should exercise first.

### 8.4 The rule `ACK-5` exists to enforce

**Hop receipt never converts to delivery, at any distance.** In A→B→C, B answering A `200/accepted`
settles A's *hop* obligation to B and moves A's sender-visible state **not at all**. Only a terminal
frame that originated at the recipient (plane C) or a terminal routing failure (§5.2, bus-emitted)
moves it. A multi-hop implementation that lets the last hop ACK stand in for the recipient ACK has
re-created the exact conflation this contract exists to prevent.

---

## 9. Wire shapes (`ACK-3`, `ACK-5`)

### 9.1 Routes

| Route | Surface | Auth | Purpose |
| --- | --- | --- | --- |
| `POST /v1/ack` | agent | session + mTLS, cross-checked (invariant 11) | A recipient issues plane-C ACK/NACK. |
| `GET /v1/ack/{correlation_key}` | agent | session + mTLS, cross-checked | The sender reads status (§13). |
| `POST /v1/peer/ack` | peer | `RequirePeerPrincipal` (§6.1) | A peer propagates a terminal outcome one hop back. |

`POST /v1/peer/ack` is a **new peer route** and MUST be mounted only through
`internal/httpapi/peermount.go` — the one permitted mount site, bounded by an existing guard test
(`internal/relay/guards_test.go`). Registering it anywhere else evades the mount guard.

**Invariant 7 is not optional here:** every one of these ships with an `agent-busctl` subcommand and
an `AGENT_PROTOCOL.md` entry **in the same task**. A capability without its subcommand is the missing
half of the task, and `curl` is not a substitute.

### 9.2 The peer ACK frame

```
{ "wire_version": 1,
  "correlation_key": "<origin-bus>-<seq>",
  "recipient":       "<bus-id>.<agent-id>",
  "outcome":         "delivered" | "refused" | "undeliverable",
  "class":           "<closed enum>",          // iff outcome is negative (§5)
  "emitted_at":      <unix ms>,
  "attestation":     { "signature": "<base64, exactly 64 bytes>" }  // recipient outcomes only
}
```

Bounded by construction: no field's length is chosen by a remote party (§5.3), so the request cap can
be derived exactly as `MaxRelayBytes` is (`internal/relay/message.go:63-80`) rather than guessed.

### 9.3 Status codes on `POST /v1/peer/ack`

Mirror the relay ingress deliberately (`internal/relay/relayhttp.go`), for the reason that file
already gives: a 4xx/5xx on a *settled, not-your-fault* outcome makes retry/backoff amplify exactly
the traffic the mechanism exists to stop.

| Situation | Answer |
| --- | --- |
| Accepted, or an idempotent replay | **200**, `{accepted:true, duplicate:<bool>}` |
| Terminal already recorded, absorbing (§8.2 note 6) | **200**, `{accepted:true, duplicate:true}` |
| No obligation binds this peer to this key (§6.2) | **409**, existing `CodeIdempotencyViolation` family — reject and log, **no disconnect** |
| Malformed / unknown class / unknown `wire_version` | **400**, existing `CodeInvalidRequest` |
| Peer not authenticated | **403**, via `RequirePeerPrincipal` |

### 9.4 Multi-hop back-propagation (`ACK-5`)

- A terminal outcome propagates **backwards along the traversed `bus_path`, one hop at a time**,
  each hop re-authenticated by §6.1 and re-bound by §6.2. **No bus contacts a bus it is not peered
  with**, and nothing in a peer-supplied frame ever names an address, host or scheme — the same rule
  the relay envelope already obeys (`CONTRACTS-HTTP.md`, "Onward relay" / "Where it can go").
- **The class and the attestation are forwarded VERBATIM.** An intermediate re-signs nothing and
  re-classifies nothing (invariant 2), exactly as it re-attests nothing when forwarding a message.
- Loop control on the ACK path is the **same** mechanism as on the message path: an ACK frame carries
  the correlation key, and §6.2's job binding means an ACK can only travel back along a route this
  bus actually wrote a job for. Idempotency (§12) absorbs a diamond; the binding bounds the graph.
- **Exactly once at the origin** falls out of §8.2 note 4: the first terminal wins, later ones are
  absorbed or rejected as violations. It does **not** require an exactly-once transport, which does
  not exist here.

---

## 10. DECISION — versioning, and the sequencing call the coordinator asked for

> **RULING: `ACK-3` MUST add a `wire_version` field to BOTH the existing relay envelope AND the new
> ACK frame, in the same change, spending the ALREADY-RESERVED `relay-wire-version = 1`. It MUST NOT
> reserve a second value, and it MUST NOT add an unversioned ACK frame.**

The facts:

- `internal/relay/message.go:25-32` states the design rule and its trigger in the code itself: "The
  relay envelope deliberately carries NO wire-protocol version field … **The task that first
  REGISTERS this handler reserves a version and adds the field to BOTH surfaces at once.**"
- **That trigger has already fired and the obligation was missed.** The handler *is* registered:
  `internal/httpapi/peermount.go:5` is the one permitted mount site for `/v1/peer/relay`, and
  `cmd/agent-bus/relaywiring.go:1082` constructs the `httpapi.PeerSurface` that mounts it.
- The version **is** reserved: Spec Server namespace `relay-wire-version`, value **1**, reserved
  2026-08-08 by spec-keeper, note *"FEDERATION phase, RELAY-23 will spend this"*. It has not been
  spent.
- `RelayedMessage` still carries no version field.

So the epic is about to add a **second** unversioned frame to a surface that already carries one — 
doubling a known defect rather than paying it down. `ACK-10` (negotiation and downgrade safety)
cannot be built on a field that does not exist, which is why this belongs to `ACK-3` and not to
`ACK-10`.

**Reading rules, so adding the field is not a break:**

- A **missing** `wire_version` on the relay envelope MUST be read as **1**. That is the only
  backward-compatible read and it is exact — value 1 *is* the format currently on the wire.
- An **unrecognised** version MUST be **rejected, never defaulted** (`parseOutboxState`'s posture,
  `outbox.go:316-330`). Downgrade behaviour is `ACK-10`'s, on top of a field that exists.
- **If the reviewer rules this outside `ACK-3`'s file boundary**, it becomes a separate task that
  `ACK-3` is **blocked by**. It does not get skipped, and `ACK-3` does not ship an unversioned frame
  in the meantime.

---

## 11. DECISION 6 — retention, and the bound that makes it safe

> **RULING: an ACK state record is retained for `AckRetention = relay.OutboxSettledRetention` (=
> `relay.RetryHorizonCeiling` = `idem.PeerOutageBudget` = **24h**), measured from `accepted_at` for a
> non-terminal record and from `settled_at` for a terminal one. The table has a hard entry cap and a
> per-agent fair share, both fail-closed: nothing is ever evicted to make room.**

### 11.1 Why that exact term, derived rather than picked

`internal/idem/retention.go:5-18` forbids a round number, and the reasoning transfers: a window is
only worth stating if it demonstrably exceeds the longest interval over which the state could still
change.

- Nothing can still be in flight past the outbox's own horizon: `NewForwarder` **refuses** options
  where `RetryHorizon + Timeout > RetryHorizonCeiling` (`internal/relay/forward.go:608-613`), and
  `RetryHorizonCeiling = idem.PeerOutageBudget` **by reference, "so the two cannot drift apart"**
  (`forward.go:70-72`).
- The per-hop tombstone already uses precisely this window:
  `OutboxSettledRetention = RetryHorizonCeiling` (`internal/relay/outbox.go:169`).
- So a **longer** window retains rows nothing can change, and a **shorter** one lets a live pending
  hop outlive its own status row — a sender would be told `unknown` about a delivery still in
  progress, which is worse than being told nothing.

**Adopting an existing constant by reference is the decision.** A fresh `AckRetention = 24h` literal
would be a second number that must agree with three others, and §3's own reasoning ("two fields that
must agree are two fields that can disagree") applies to constants too.

### 11.2 The cap, and what happens at it

Mirror `internal/idem/retention.go:99-112` and `:228-241` exactly:

- **Hard entry cap**, derived from a memory budget divided by the worst-case record footprint — which
  is computable because §7.2 has no variable-length field.
- **Per-agent fair share** on the same `maxEntries / (agents + 1)` rule, engaged above a
  `maxEntries/2` pressure line, so one authenticated agent cannot deny status to every other agent.
- **Nothing is evicted.** Evicting a live ACK row turns a real terminal outcome into a false
  `unknown` — an *inversion* of the truth, not a gap in it.

### 11.3 The one place this contract deliberately degrades instead of failing closed

> **When the ACK table is at capacity, the SEND STILL SUCCEEDS (201) and the ACK row is NOT created.
> `GET /v1/ack/...` then reports `unknown`. The refusal is counted and logged loudly and specifically
> (invariant 6).**

Stated plainly because it is the one asymmetry: everywhere else in this repo the fail-closed answer
is to refuse the operation. Here refusing would mean an **observability** table causing a
**messaging** outage — and worse, it would violate nothing while breaking everything, since the
message was already durable and the sender was already told 201 (invariant 4). Degrading the
*observation* is recoverable; refusing the *send* is not. The loud log is what stops this being
silent, and it is the same trade invariant 6 makes for a damaged log.

---

## 12. Idempotency, and the disconnect question answered explicitly (`ACK-4`, `ACK-7`)

> **RULING: NO new disconnect is introduced anywhere in the ACK plane. Every refusal is
> reject-and-log.**

Invariant 10 requires two questions to be answered before *any* disconnect is added. Both are
answered here, on the record:

1. **Can a merely BUGGY client reach this line?** **Yes, trivially** — an agent that ACKs a
   correlation key it mistyped; an agent that re-ACKs after its own restart; a peer that re-sends an
   ACK because our 200 was lost in flight. All three are honest.
2. **Does this connection carry only ONE principal's traffic?** **No.** A peer bus relays for every
   agent behind it. This is the case invariant 10 says "becomes load-bearing the moment relay ingest
   lands" — and relay ingest **has** landed (`internal/httpapi/peermount.go`,
   `cmd/agent-bus/relaywiring.go:1082`). Dropping that socket punishes every bystanding agent behind
   the peer.

Both answers point the same way, so the rule is absolute rather than a judgement call. **`ACK-4` MUST
carry a test that asserts no ACK-plane refusal drops a connection.**

The three cases, never collapsed:

| Case | Answer |
| --- | --- |
| Same key + **same** outcome | Legitimate retry. Return the ORIGINAL result, re-apply nothing, do not error, **do not disconnect**. |
| Same key + **different** terminal outcome | Protocol violation. Reject **and log**, **do not disconnect**. This is the outbox monotonicity rule (`outbox.go:1685`, `:1702`). |
| Replay of an already-accepted **signed** message | Untouched by this epic. It is the message plane's rule and it is the only disconnect on the bus. **An ACK frame is not a message and must never reach that path.** |

**The `409 no-matching-reservation` indistinguishability is not to be "fixed" here.** A test asserts
it (invariant 10) and it goes RED the day it becomes resolvable. §13.3's uniform answer is a
*deliberate* analogue of the same posture, not an accidental copy of it.

---

## 13. Sender-visible status (`ACK-9`)

### 13.1 The route

`GET /v1/ack/{correlation_key}` → one row **per recipient** (§3.2), plus `?wait=<duration>` for the
long-poll variant, bounded by the same ceiling as any parked poll (`hub.MaxPollTimeout`, 5 minutes —
restated as `idem.ParkedPollMax`, `internal/idem/retention.go:47`).

### 13.2 The row

```
{ "correlation_key": "...", "recipient": "...",
  "state":       "accepted"|"in_flight"|"delivered"|"refused"|"undeliverable"|"unknown",
  "class":       "<closed enum>",     // present iff state is a negative terminal
  "attested_by": "peer_bus"|"recipient_signature_unverified",   // §6.3 — never "verified"
  "accepted_at": "...", "settled_at": "..." }
```

### 13.3 Authorization, and the status-oracle rule

> **Only the ORIGINAL SENDER may read a row. Every other case — key never existed, key swept, key
> belongs to someone else — returns the SAME answer: `200` with `state: "unknown"`.**

A `403` would confirm the message exists, which is the oracle `ACK-4` is required to close. This is
the same reasoning `handleBroadcast` already applies when it authenticates before answering 501 — "a
route that told an ANONYMOUS caller what it does and does not implement would be describing the
messaging surface to somebody with no business knowing it exists"
(`internal/httpapi/messages.go:456-460`).

The response also **MUST NOT** disclose the traversed `bus_path`, the peer bus that refused, the
recipient's poll activity, or anything about the recipient's roster membership. The sender learns the
outcome for recipients **it named** and nothing else about the federation.

### 13.4 CLI (invariant 7)

`agent-busctl ack-status <correlation-key> [--wait] --json`, plus `agent-busctl ack <message-id>
[--refuse <class>]` for the recipient side. Exit codes reuse the existing stable set
(`client/errors.go:95-109`) — **no new code is minted**:

| Situation | Exit |
| --- | --- |
| Reported a state successfully (any state) | `ExitOK` (0) |
| `--wait` reached `delivered` | `ExitOK` (0) |
| `--wait` reached a negative terminal | `ExitRejected` (7) |
| `--wait` and the state is `unknown` | `ExitEmpty` (8) |

---

## 14. GOLDEN PATH DEFERS — clauses that are specified here but NOT implemented in this run

The operator scoped this run to: **buses do not crash, the network is up end to end.** Each clause
below is part of the contract and is **deferred**, never "not needed".

| # | Clause | Deferred to |
| --- | --- | --- |
| D1 | Reconstructing ACK state after a crash at any transition boundary; proving restart yields the same terminal state with no resurrection and no index reuse. | **`ACK-8`** |
| D2 | Detecting and emitting `obligation_lost` (§5.2). **Today, a pending onward hop is durably ABANDONED at restart** — `RELAY-48`: the intermediate does not retain the origin's attestation, so `Resume` cannot rebuild the envelope and settles the job abandoned (`internal/relay/forward.go:1496-1498`). | **`ACK-8`**, and `RELAY-48` must land first |
| D3 | Durable forwarder queue. The per-peer queues are **in-memory and lossy**; `Dropped.Full`, `Dropped.Expired` and `Dropped.Yielded` are all silent loss paths (`RELAY-2-FU-DURABLE-OUTBOX`, `internal/relay/forward.go:118-135`). | `RELAY-2-FU-DURABLE-OUTBOX` |
| D4 | Retry/backoff of ACK/NACK frames. `ACK-7` keeps **idempotency** and drops the retry machinery in this run. | `ACK-7` (post-golden-path) |
| D5 | Version **negotiation** and downgrade safety. The version **field** is NOT deferred — it is `ACK-3`'s, §10. | **`ACK-10`** |
| D6 | Broadcast aggregation. Blocked on `SIGN-3` defining a canonical audience. §5.5 fixes the two constraints so no schema change is needed later. | `SIGN-3`, then a new task |

### 14.1 What a sender is told TODAY when a message is lost to the non-crash-safe relay

Answered directly, because the brief requires it and because a contract that is vague here is worse
than none:

- **Under this contract, once D2 lands:** terminal `undeliverable`, class `obligation_lost`. The
  sender is told, and told *why*, in the same closed vocabulary as every other failure.
- **Today, and for the whole golden-path run:** the sender is told **nothing**. The row stays
  `in_flight` until §11's 24h window sweeps it, after which `GET /v1/ack/...` answers `unknown`. The
  loss **is** logged loudly and specifically on the intermediate bus (invariant 6 holds — `RELAY-48`
  confirms the discard is logged at WARN), so it is diagnosable by an operator and invisible to the
  sender.
- **`ACK-11` MUST document that gap in exactly those terms.** "in_flight forever, then unknown" is
  the honest description, and it is the single strongest argument for prioritising `ACK-8` and
  `RELAY-48` immediately after this run.

---

## 15. Acceptance for `ACK-12` (three-bus smoke)

Reuse `DEPLOY-3`'s Compose topology and `scripts/fed-smoke.sh`; **do not build a parallel harness.**

**CORRECTED 2026-08-16 — the "landmine" this section originally described DOES NOT EXIST, and the
original text is preserved nowhere because it was simply wrong.** It claimed `fed-smoke.sh` asserts
the same `message_id` across A's, B's and C's audits and that this is the only reason the script
exits 1. Checked against the script at HEAD: **it correlates on the audit record's
`content_sha256`**, and its own CROSS-BUS CORRELATION block (`scripts/fed-smoke.sh:257-268`) already
explains that a message id is not a cross-bus correlator and says **"Do not reintroduce it."** The
script exits **0**. `message_id` appears only as a per-bus selector against the bus that MINTED it.

The claim was refuted on the `ACK-1` task journal the day this document was written; the correction
did not reach the document until now, which is precisely the stale-note failure mode
(`CONTEXT-STALE-NOTYET`) this repo has spent the day removing — a reader sees the file, not the
journal.

**What survives, and it is the part that matters:** `ACK-12` MUST correlate on the correlation key
(§3), never on a local `message_id`, because under invariant 1 each bus mints its own id and they
are not equal across hops. `fed-smoke.sh` already does the equivalent for message bodies. This
contract's choice of key is what makes an ACK-level assertion writable in the same style. **Do not
"fix" `fed-smoke.sh`** — it is correct.

Minimum assertions: local acceptance is durable and distinct from hop receipt; a hop ACK does **not**
move the sender-visible state (§8.2 note 3); a recipient ACK propagates A←B←C and lands terminal
**exactly once**; a recipient NACK carries a class from §5.2 and **no free text**; a duplicate ACK
frame is absorbed with no second effect and **no disconnect**; the status API answers `unknown`
uniformly for a key that is not the caller's.

**Proof-command hygiene:** `ACK-12`'s stored `proof_cmd` names `./tests/e2e`, and **no `tests/`
directory exists in this repository** (verified). `ACK-3` and `ACK-4` already have `proof_cmd`s that
are **UNVERIFIABLE BY CONSTRUCTION** — their unquoted `-run` regex parens/pipes are re-parsed as
shell metacharacters by `proof-check.sh`'s inner `bash -c` (task `0fb4d032`). Those must be repaired
before any ACK task is completed on them; a task may not be completed on a VACUOUS proof, and one
that cannot run at all is worse.

---

## 16. Open questions — flagged, not guessed

| # | Question | Why it is not answered here | Who should answer |
| --- | --- | --- | --- |
| **Q1** | Nothing distributes agents' messaging public keys, so **no party can verify a layer-3 recipient attestation** — not the bus (by design) and not the sender (for want of a key). | Answering it means designing a key-distribution endpoint, which is a `CRYPTO`/`SIGN` concern and far outside `ACK-1`. Guessing a shape would bind eleven tasks to it. | A new task; `ACK-11` must document the limitation meanwhile. |
| **Q2** | Should a **terminal negative** ACK cancel outstanding hops for that recipient (stop retrying a message the recipient already refused)? | It is a genuine efficiency/correctness trade — cancelling saves work, but a NACK from one route does not prove the other route's copy is unwanted, and cancelling on an unverifiable attestation (Q1) is a denial-of-delivery vector. | `ACK-7`, with `ACK-4` reviewing the DoS angle. Default until then: **do not cancel**. |
| **Q3** | Does `POST /v1/ack` need its own **rate limit** distinct from the idempotency fair share? A recipient can emit one ACK per message it receives, which is bounded — but an agent can also ACK keys it invents, which §13.3's uniform answer makes cheap to probe. | The abuse-control primitive for a multi-principal surface is an open question already filed under `RELAY` (`48223968`). Duplicating a decision here would fork it. | `ACK-4`, coordinated with `48223968`. |
| **Q4** | `/v1/leave` does not exist and sessions are memory-only (`client/client.go:675-686`). Should an agent leaving invalidate its outstanding ACK obligations? | There is no leave event to hook, so the question is unanswerable rather than merely unanswered. | Whichever task lands `/v1/leave`. |

---

## 17. Latent landmines found while writing this

1. **`PROTOCOL.md` §8.5 and §10 are STALE.** Both say `internal/relay` "registers no handler on any
   mux", is "served by nothing", and that "no relayed signature is verified in production and no
   cross-bus message flows at all today" (`PROTOCOL.md:931-946`). **That is no longer true** —
   `internal/httpapi/peermount.go` mounts the peer routes and `cmd/agent-bus/relaywiring.go:1082`
   constructs the surface. Per `CLAUDE.md`'s own warning, a stale "not yet implemented" note is more
   dangerous than none because it reads as freshly checked. **`ACK-11` should correct it**; this
   contract cites code, never those paragraphs.
2. **The `relay-wire-version` obligation is outstanding.** §10. The precondition
   (`internal/relay/message.go:25-32`) fired when the handler was mounted and the field was never
   added.
3. **`store.Message.OriginMessageID` had no production writer as of `RELAY-48`'s filing** — the field
   exists but `Store.byOrigin` was empty, which is the mechanism behind D2. **`ACK-2` MUST verify
   whether a writer now exists before relying on `OriginID()` returning an origin id on an ingested
   message.** This contract's correlation key is correct either way (on the origin bus, `OriginID()`
   returns `ID`), but multi-hop correlation depends on it.
4. **`ACK-3`, `ACK-4` and `ACK-12` proof commands cannot pass for any implementation.** §15.
5. **Broadcast tests skip in bulk.** ~31 broadcast tests skip while `/v1/broadcast` is 501
   (`DECISIONS.md` 2026-08-08 (c)). A green suite is **not** evidence that anything broadcast-shaped
   works — do not read one as clearing §5.5.

---

## 18. Invariant compliance

| Inv. | How this contract satisfies it |
| --- | --- |
| **1** | The correlation key is server-minted **by the origin bus** and never client-supplied (§3). No id is reused; no new id is minted at all. A client-supplied key on the status API is validated input, never identity. |
| **2** | Every agent id in the record and on the wire is fully qualified (§7.2, §9.2). An intermediate re-classifies and re-attests nothing (§9.4). |
| **4** | Every state transition is committed and fsynced **before** it is reported (§1.1, §7.1). The 2026-08-02 narrowing is cited as scope, never as licence. |
| **5** | The ACK table is a serving copy rebuilt by replaying the durable record; disk is the truth (§7.1). Recovery reconstruction is **deferred to `ACK-8`**, explicitly (§14 D1). |
| **6** | **No free text anywhere** — a closed class enum only (§5). `OutboxRecord.Reason` never reaches the wire or a sender. Every discard/refusal (§11.3, §12, §14.1) is logged loudly and specifically. |
| **10** | Three cases never collapsed (§12). **No new disconnect**, with both mandated questions answered on the record. The `409` indistinguishability is preserved, not "fixed". |
| **11** | Peer frames authenticate by certificate through `RequirePeerPrincipal`; agent frames require session **and** mTLS, cross-checked. `peermount.go`'s documented one-factor **narrowing** is inherited and explicitly not widened (§6.1). |
| **7** | Every route ships an `agent-busctl` subcommand and an `AGENT_PROTOCOL.md` entry in the same task (§9.1, §13.4). No `scripts/bus-*.sh` wrapper; no hand-written HTTP. |
| **9** | No new primitive. Reuses `internal/signing`'s existing scheme and framing; a new *encoding* only. Explicitly forbids a bespoke MAC/KDF/construction and any reuse of the WAL MAC key (§6.3). |

---

_Written for `ACK-1`. Design only — no production code was written or changed by the task that
produced this file._
