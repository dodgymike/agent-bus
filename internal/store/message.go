package store

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// RecordKind is the wal.Entry.Kind discriminator for a message. wal does not
// interpret it; it exists so a replay can tell a message record from the
// enrolment records AUTH-3 will add alongside them.
const RecordKind = "message"

// RecordVersion is the schema version of the JSON payload inside a message
// wal.Entry. It is NOT the on-disk WAL format version (that is reserved and
// owned by internal/wal) and it is NOT the HTTP API version: it versions only
// the field set of Record below.
//
// The number is RESERVED, never chosen by eyeballing this constant:
//
//	store.RecordVersion = 2 — RESERVED 2026-08-07 from the Spec Server
//	`store-record-version` namespace by feature-runner (value 1 was seeded to
//	cover the already-shipped v1 record).
//
// Bumping it is a last resort. Record is deliberately shaped so that the CRYPTO
// epic and DUR-5 can ADD optional fields without a break — see the Record doc —
// and an added optional field does not move this number.
//
// # Why SIGN-6 moved it anyway, and what that costs the operator
//
// v2 adds "timestamp_ms" and "signature", and both are REQUIRED rather than
// optional: SIGN-6 admits no unsigned message, so a record carrying neither is
// not an older shape of the same fact, it is a message this build is not
// willing to say was signed. Decode therefore REFUSES every v1 record, Hub.Apply
// logs the refusal loudly and skips it, and an operator upgrading an existing
// bus must expect its message history to be DISCARDED.
//
// There is no migration and there cannot be one: pre-SIGN-6 messages are
// unsigned, and the only way to give them a v2 shape would be to invent a
// signature or to mark them "signed by nobody" — the first is forgery, the
// second is the unsigned-traffic hole SIGN-6 exists to close. Enrolment
// ("agent") and invite records are untouched: this version covers Record and
// nothing else.
const RecordVersion = 2

// MaxBodyBytes bounds a single message body.
//
// It is far below internal/wal's MaxPayloadSize (1 MiB) on purpose: the durable
// record is the body PLUS its routing metadata plus JSON and base64 overhead,
// and base64 alone costs 4/3. 64 KiB of body encodes to ~88 KiB, which leaves
// an order of magnitude of headroom under the frame limit for the metadata, for
// the recipient list, and for the encrypted-envelope descriptor the CRYPTO epic
// will add.
//
// A bus is for coordination between agents, not for shipping payloads: an agent
// with a megabyte to share should send a reference to it.
const MaxBodyBytes = 64 << 10

// MaxRecipients bounds the recipient list of a single directed message.
//
// Today /v1/send takes exactly one recipient, so the only value that occurs is
// 1. The bound exists because Record is decoded from DISK during recovery,
// where the input is whatever the file holds rather than whatever a handler
// validated, and an unbounded list there is unbounded memory on the startup
// path. The slack above 1 is for the multi-recipient send the RELAY epic needs.
const MaxRecipients = 64

// MaxIdempotencyKeyLen bounds the client-supplied key carried in a durable
// record. It matches hub.MaxIdempotencyKeyLen and auth.MaxIdempotencyKeyLen —
// the same value at all three boundaries, restated here because this one
// validates bytes off DISK rather than off a request.
const MaxIdempotencyKeyLen = 128

// MaxBusPath bounds the traversed-bus list. It is the loop-prevention field
// invariant 6 names, so a long path is a routing pathology rather than a
// legitimate topology; 64 hops is far past any bus mesh worth building and is
// finite, which is the point on a field decoded from disk and echoed to
// clients.
const MaxBusPath = 64

// MaxReceivedBusPath is the largest path a bus may accept from a peer. The
// receiving bus appends itself before persistence, so the wire boundary is one
// hop smaller than MaxBusPath. Relay and hub ingress share this constant so a
// path accepted at the HTTP boundary cannot be refused at durable ingest.
const MaxReceivedBusPath = MaxBusPath - 1

// Sentinel errors. All are checkable with errors.Is.
var (
	// ErrInvalidMessage reports a Record that cannot be turned into a Message:
	// a bad sequence, a malformed id, an oversized body, a content hash that
	// does not match. It is returned both by the validating decoder on the
	// recovery path and by the constructor on the write path.
	ErrInvalidMessage = errors.New("store: invalid message record")

	// ErrDuplicateSequence reports an Append whose sequence is ALREADY held by
	// the serving copy. It replaced ErrOutOfOrder in
	// SIGN-1-FU-OUTOFORDER-POISON.
	//
	// (a) What it means: this exact sequence has already been applied. Either a
	// replay is folding one durable entry in twice, or the write path handed the
	// same number out twice.
	//
	// (b) It is an INVARIANT 1 breach — ids and sequence numbers are minted by
	// the server and are NEVER reused — so it stays LOUD, and a hub that
	// poisons itself on it is RIGHT to. This is the one failure Append exists to
	// catch, and relaxing the ordering rule did not relax it.
	//
	// (c) "Behind the head" is NO LONGER an error, and must not be turned back
	// into one. SIGN-1 made a send two-step: hub.Mint allocates and durably
	// burns a sequence so the CLIENT can sign it, and only then does the client
	// send. Reservations live for hub.MintTTL, so two agents routinely hold
	// numbers at once and spend them in either order. The old rule (strictly
	// greater than the head) was only ever true because the sequence used to be
	// allocated immediately before the append, under the same lock — commit
	// order equalled allocation order by construction. It no longer does. Seq is
	// now a PRE-ASSIGNED IDENTITY, not a commit-order position, and refusing a
	// late arrival here rejects a record the hub has ALREADY committed and
	// fsynced (invariant 4), which orphans it on disk and poisons the bus.
	ErrDuplicateSequence = errors.New("store: message sequence has already been applied")
)

// Message is one message in the serving copy.
//
// Every field is SERVER-DERIVED. Sender is the authenticated principal, never a
// value from the request body; Seq and ID are minted by the server (invariant
// 1); SentAt is the server's clock. A client supplies exactly two things — the
// body and, for a directed message, the recipient — and both are validated
// before they reach here.
type Message struct {
	// Seq is the server-minted sequence number. It is the message's IDENTITY —
	// the number the id derives from and the number the SENDER SIGNED — and it
	// is NOT the delivery order. Since SIGN-1 a sequence is minted and durably
	// burned before the client signs and sends, so two agents holding
	// reservations spend them in whatever order they please. See Pos.
	Seq uint64

	// Pos is the server-assigned DELIVERY POSITION: the WAL commit index of the
	// record that made this message durable. It is what a cursor points at, what
	// Store keeps the serving copy ordered by, and what Since binary-searches.
	//
	// It is monotone, never reused and stable across restart — replay folds
	// records in commit order, so a recovered message is assigned the same
	// position it had before — but it is NOT part of the durable record and does
	// NOT appear in Record, in the JSON, or in the audit trail. It is DERIVED
	// from where the record sits in the log, which is why splitting it out of Seq
	// cost no on-disk format change and did not move RecordVersion.
	//
	// Zero means UNSET. Store.Append refuses it: 0 is the reserved "I have seen
	// nothing" cursor value, so a zero stamp would sit below every cursor and
	// replay a reader's whole retention window.
	//
	// Do not conflate the two counters. Seq feeds the mint sequence floor
	// (invariant 1: never reused, never rewound); Pos feeds delivery. A late,
	// low Seq gets a HIGH Pos, lands above every cursor, and is therefore
	// delivered to — and wakes — every reader (SIGN-1-FU-REORDER-WATERMARK).
	Pos uint64

	// ID is the fully-qualified message id "<bus-id>-<seq>" (ids.MessageID).
	ID string

	// OriginMessageID is the id the ORIGIN bus minted for this message. It is
	// the THIRD notion on this type and it is not a variant of either of the
	// other two — read this before adding anything that looks like it:
	//
	//	Seq                IDENTITY. Server-minted, client-signed, spendable out
	//	                   of order (see Seq).
	//	Pos                DELIVERY POSITION. The WAL commit index; what cursors
	//	                   point at and what Store orders by (see Pos).
	//	OriginMessageID    CORRELATION KEY. It answers "which message on the
	//	                   ORIGIN bus is this a local copy of", and NOTHING else.
	//
	// IT TAKES PART IN NO ORDERING, NO CURSOR AND NO RETENTION DECISION. It is
	// never compared, never sorted on, never used to decide what a reader may
	// see next and never used to decide what ages out. Carelessly adding a
	// fourth ordering axis to this type is precisely how
	// SIGN-1-FU-OUTOFORDER-POISON happened; this field is deliberately inert.
	//
	// It is set ONLY when this bus INGESTED the message over a relay hop, via
	// WithOriginMessageID. It is EMPTY when this bus is the origin — in which
	// case ID already IS the origin id, and OriginID() is the one place that
	// rule is written down. It is never a duplicate of ID: two fields that must
	// agree are two fields that can disagree, which is the reason
	// internal/relay's outbox carries one origin id rather than a bus and a
	// sequence, and the reason Record deliberately omits Pos.
	//
	// It is NEVER this message's identity. This bus mints its own id and never
	// adopts a peer's (invariant 1).
	OriginMessageID string

	// OriginAttestation is the ORIGIN bus's signed binding of Sender to Sender's
	// MESSAGING public key, carried VERBATIM from the hop that delivered this
	// message. Like OriginMessageID it is present ONLY on a message this bus
	// INGESTED over a relay hop; the ZERO value means "this bus is the origin",
	// which is not an error but the ordinary case for every message this bus
	// minted itself. Set through WithRelayOrigin, which sets it together with
	// OriginMessageID because the two are useless apart (see there).
	//
	// # WHY IT MUST BE DURABLE — RELAY-48
	//
	// It is the ONE field an ONWARD hop needs that nothing else on this type can
	// supply. Everything else the next hop's envelope requires is already here
	// (Sender, Recipients, BusPath, TimestampUnixMilli, Signature, Body,
	// ContentSHA256, and the origin bus and sequence, which are the two halves of
	// OriginMessageID) — but relay's ValidateRelayRequest REQUIRES an origin
	// attestation and this bus CANNOT mint one: attest.Sign refuses a subject in
	// another bus's namespace (invariant 2), and re-attesting somebody else's
	// agent is the one thing the federation-trust design forbids outright.
	//
	// So without it a relayed-in envelope is unbuildable from durable state, and
	// a pending onward hop that survives to the next boot as an outbox job is
	// settled ABANDONED — after this bus already answered the upstream peer 200.
	// That is invariant 4's promise broken in spirit: we accepted the obligation
	// durably and then destroyed it. It is not enough for the value to be in
	// memory, because its only reader (relay.Forwarder.Resume) runs ONLY after a
	// restart, which is precisely when memory is gone.
	//
	// # IT IS AUTHENTICITY METADATA, NOT A BODY (invariant 6)
	//
	// It names an agent, a public key, an epoch and two timestamps, and it is
	// signed by a bus. It carries no part of the message's content and cannot be
	// used to recover any: the body is covered by Signature, not by this. It is
	// therefore routing/authenticity metadata of exactly the kind invariant 6
	// sanctions — and note that it does NOT reach the AUDIT log at all, because
	// wal.AuditRecord is assembled field by field in hub's auditRecordFor and
	// this field is not among them.
	//
	// It is INERT in every ordering, cursor and retention decision, for the same
	// reason OriginMessageID is: it is never compared, never sorted on, and never
	// consulted to decide what a reader may see or what ages out.
	OriginAttestation attest.Attestation

	// Sender is the fully-qualified "<bus-id>.<agent-id>" of the authenticated
	// sender (invariant 2).
	Sender string

	// Broadcast reports a message addressed to the whole bus rather than to a
	// named recipient. It is stored as a FLAG rather than as an expanded
	// recipient list on purpose: expanding it would freeze the roster as it
	// stood at send time into the durable record, which is both larger and
	// wrong — an agent that enrols later and reads back through the retention
	// window should see the broadcast, and a roster snapshot cannot express
	// that.
	Broadcast bool

	// Recipients holds the fully-qualified ids a directed message is addressed
	// to. It is empty for a broadcast.
	Recipients []string

	// BusPath is the ordered list of bus ids this message has traversed,
	// starting with the bus that accepted it. It is the loop-prevention and
	// provenance field invariant 6 names, and the RELAY epic appends to it.
	BusPath []string

	// SentAt is when this bus accepted the message, by the server's clock.
	SentAt time.Time

	// Body is the opaque payload. The bus NEVER interprets it: it is carried
	// and hashed as bytes so that the CRYPTO epic can put ciphertext here
	// without anything on this path changing.
	Body []byte

	// ContentSHA256 is the hex SHA-256 of Body. It is what lets the audit log
	// (DUR-5) prove WHAT was sent without retaining it — invariant 6 keeps
	// bodies out of the audit trail, and a hash is the compromise that keeps
	// the trail useful once payloads are end-to-end encrypted.
	ContentSHA256 string

	// IdempotencyKey is the client-supplied key this message was accepted
	// under (invariant 10). It is durable and is rebuilt into the applied-key
	// table on replay, which is what makes the idempotency memory survive a
	// restart rather than being an in-memory cache.
	IdempotencyKey string

	// TimestampUnixMilli is the SENDER's clock, Unix milliseconds UTC. It is
	// COVERED BY THE SIGNATURE and is therefore NOT interchangeable with SentAt,
	// which is this bus's clock and is not covered.
	//
	// Keeping both is the point, not redundancy. SentAt is what this bus will
	// swear to and is what retention and ordering are computed from; this field
	// is what the SENDER claimed and is the only one a recipient can check
	// against the signature. A bus that collapsed them would either be signing
	// its own clock (which the sender never saw) or ordering on a clock a client
	// chooses. Clocks lie, so this is not a freshness mechanism either.
	//
	// NOR IS THE SEQUENCE (corrected 2026-08-14, SIGN-1-FU-REORDER-WATERMARK —
	// the previous wording here named "the server-minted monotonic sequence plus
	// the recipient's cursor", and a recipient built from that sentence
	// re-implements the very suppression that task fixed). Replay protection is
	// enforced SERVER-SIDE AT INGEST, by refusing an already-accepted signed
	// message (invariant 10). Seq is minted when a client RESERVES, not when it
	// sends, so it is NOT monotone in delivery order and is an IDENTITY rather
	// than a freshness token; the recipient's cursor is a delivery POSITION (see
	// Message.Pos), which is a different number and is not comparable to Seq.
	TimestampUnixMilli int64

	// Signature is the sender's 64-byte detached Ed25519 signature over
	// signing.Canonicalize of this message. The bus carries it as OPAQUE BYTES
	// and never verifies it.
	//
	// That is a deliberate division of labour, not a gap: the bus does not hold
	// the sender's messaging key and must not be trusted to police messages for
	// senders it does not control, so the BUS enforces SHAPE (present, exactly
	// signing.SignatureSize bytes) and the RECIPIENT enforces AUTHENTICITY.
	// SigningMessage below is the one place the mapping from this record to the
	// bytes this signature covers is written down.
	Signature []byte
}

// Size reports the body size in bytes, which is the size the audit trail
// records and the number retention accounts against.
func (m Message) Size() int { return len(m.Body) }

// OriginID returns the id the ORIGIN bus minted for this message:
// OriginMessageID when it is set, and ID otherwise.
//
// THIS IS THE ONE PLACE THE RULE IS WRITTEN DOWN. A caller that spells it out
// itself (branching on OriginMessageID being non-empty) is a second copy of a
// rule that can drift
// from this one, and the drift is silent — the wrong branch still returns a
// well-formed message id, it just names the wrong bus's message, so a relay
// correlation would resolve to nothing and a resume would abandon a job that was
// perfectly recoverable.
//
// It is a CORRELATION key, not an identity: it is not this message's id on this
// bus and must never be served as one, stamped onto a record, or handed to a
// client as "the message id" (invariant 1 — this bus never adopts a peer's id).
func (m Message) OriginID() string {
	if m.OriginMessageID != "" {
		return m.OriginMessageID
	}
	return m.ID
}

// WithOriginMessageID returns a COPY of m carrying originMessageID.
//
// # IT SETS THE CORRELATION KEY AND NOTHING ELSE — THE RELAY INGEST WANTS
// # WithRelayOrigin
//
// A relay ingest needs TWO durable facts, and this method writes only one of
// them. Setting the id alone produces a message that Store.ByOriginMessageID can
// FIND after a restart and that nothing can then REBUILD AN ENVELOPE FOR, because
// the origin attestation the next hop requires is missing (see
// Message.OriginAttestation). That is not a smaller version of the fix for
// RELAY-48, it is the SAME defect wearing a different reason string: the onward
// job stops being abandoned for "no such message" and starts being abandoned for
// "could not be read back". Both destroy an obligation this bus already answered
// 200 for.
//
// So this method survives as the ID-ONLY setter for tests and for callers that
// genuinely have nothing else to record, and the INGEST PATH MUST CALL
// WithRelayOrigin.
//
// # Why a setter and not a constructor parameter
//
// The origin id is known to the RELAY INGEST, which builds its message through
// NewMessageWithBusPath — a constructor with eleven parameters whose callers
// live in another package. Adding a twelfth would touch every local send site to
// pass "" for a field only a relay ingest can populate. This is additive
// instead: the constructors are unchanged and the ingest path opts in.
//
// # What is refused, and why each refusal is load-bearing
//
// All refusals wrap ErrInvalidMessage.
//
//   - An EMPTY originMessageID. The zero value already means "this bus is the
//     origin", so an explicit clear is not a no-op — it would ERASE provenance
//     on a message that arrived over a hop, leaving a durable record that claims
//     the message originated here. That claim is unfalsifiable afterwards.
//   - An origin id that does not parse (ids.ParseMessageID). It reaches disk and
//     a log line, and it came from a peer.
//   - An origin id whose BUS HALF equals the bus half of m.ID. A message this
//     bus minted is its own origin, and recording that twice creates a second
//     copy free to disagree with the first. THIS REFUSAL IS LOAD-BEARING: it is
//     exactly what makes Store.ByOriginMessageID's fallback to ByID sound — see
//     the soundness argument there.
//   - A DIFFERENT value when one is already set. A message has exactly one
//     origin. Setting the SAME value again is idempotent and returns nil, so a
//     retry of an ingest step is not punished (invariant 10: same key, same
//     payload is a legitimate retry).
//
// The returned copy SHARES Body, Recipients and BusPath with the receiver, and
// that is safe rather than an oversight: a Message is immutable after
// construction — NewMessageWithBusPath copies every slice on the way in, Append
// stores that copy, and copyMessage copies again on the way out — so no holder
// of either value mutates the shared arrays.
func (m Message) WithOriginMessageID(originMessageID string) (Message, error) {
	if originMessageID == "" {
		return Message{}, fmt.Errorf("%w: refusing to set an EMPTY origin message id on %s; the zero value already means \"this bus is the origin\", so an explicit clear would erase the provenance of a message that arrived over a relay hop", ErrInvalidMessage, m.ID)
	}
	originBus, _, err := ids.ParseMessageID(originMessageID)
	if err != nil {
		return Message{}, fmt.Errorf("%w: origin message id: %s", ErrInvalidMessage, err)
	}
	localBus, _, err := ids.ParseMessageID(m.ID)
	if err != nil {
		return Message{}, fmt.Errorf("%w: this message's own id: %s", ErrInvalidMessage, err)
	}
	if originBus == localBus {
		// Both halves have parsed, so both are bounded, charset-checked bus ids
		// and safe to echo.
		return Message{}, fmt.Errorf("%w: origin message id %q names THIS bus (%q), but a message this bus minted is its own origin: recording that twice is a second copy of one fact, free to disagree with %s", ErrInvalidMessage, originMessageID, localBus, m.ID)
	}
	if m.OriginMessageID != "" && m.OriginMessageID != originMessageID {
		return Message{}, fmt.Errorf("%w: message %s already carries origin message id %q; a message has exactly one origin and %q is a different one", ErrInvalidMessage, m.ID, m.OriginMessageID, originMessageID)
	}
	out := m
	out.OriginMessageID = originMessageID
	return out, nil
}

// WithRelayOrigin returns a COPY of m marked as INGESTED OVER A RELAY HOP: it
// carries the origin bus's message id AND the origin bus's attestation for the
// sender, and it is the ONE call the relay ingest path makes.
//
// # WHY THE TWO FIELDS ARE SET TOGETHER AND NOT SEPARATELY (RELAY-48)
//
// They are useless apart. The id is what Store.ByOriginMessageID resolves after
// a restart; the attestation is what makes the message the id resolves to
// re-sendable. A message with the id and no attestation is FOUND and then
// ABANDONED, which is the exact defect this method exists to close — and a
// message with an attestation and no id is never found in the first place. Two
// setters would be two chances to do half of it, and the half-done state is
// invisible until a crash, because the only reader of either field runs at
// startup. One setter makes the half-done state unrepresentable through this
// package's exported surface.
//
// # What is refused, and why
//
// All refusals wrap ErrInvalidMessage. The ID half's refusals are
// WithOriginMessageID's, unchanged and reused rather than restated — a second
// copy of that rule is a second copy free to disagree with it. The attestation
// half adds:
//
//   - A ZERO or malformed attestation, judged by attest.Canonicalize, which owns
//     the field bounds and is the same judgement the wire path makes
//     (relay.validateOriginAttestation). An attestation nobody can canonicalize
//     is one no signature can ever have covered.
//   - A signature that is not exactly signing.SignatureSize bytes. The bytes are
//     NOT echoed on refusal: they came from a peer and are on their way to a log
//     line, and the length is the whole of the fault.
//   - An attestation whose SUBJECT is not m.Sender. The next hop checks exactly
//     this equality (relay.PeerStore.AttestedSignerKey passes m.Sender as
//     attest.Subject.FQAgentID), so storing a mismatched pair would durably
//     record an envelope that can never verify anywhere.
//
// THIS PACKAGE DOES NOT VERIFY THE ATTESTATION'S SIGNATURE, and that is not an
// omission: verification needs the ORIGIN BUS'S PINNED SIGNING KEY, which lives
// in the relay peer store and never comes near the durability layer. The relay
// ingress has ALREADY verified it before a RelayedMessage exists at all
// (relay.ValidateRelayRequest runs relay.VerifyRelayed). What is checked here is
// shape and binding — the same posture as Message.Signature, which this package
// also bounds and never verifies.
//
// The attestation's two byte slices are COPIED, so the durable message cannot be
// mutated through the caller's copy after this returns. That is the same
// time-of-check/time-of-use concern internal/relay's cloneAttestation exists for,
// applied at the boundary where the value stops being transient and starts being
// durable.
func (m Message) WithRelayOrigin(originMessageID string, originAttestation attest.Attestation) (Message, error) {
	out, err := m.WithOriginMessageID(originMessageID)
	if err != nil {
		return Message{}, err
	}
	if err := validateOriginAttestation(originAttestation, m.Sender); err != nil {
		return Message{}, err
	}
	out.OriginAttestation = cloneAttestation(originAttestation)
	return out, nil
}

// HasOriginAttestation reports whether m carries the ORIGIN bus's attestation
// for its sender — that is, whether an ONWARD relay envelope can be rebuilt from
// this message alone.
//
// It exists so that the recovery seam in cmd/agent-bus asks THIS package the
// question instead of writing its own "is it zero" test, which would be a second
// copy of a rule free to disagree with Record's (see attestationIsZero). FALSE is
// the ordinary answer for every message this bus originated, and is NOT an error:
// such a message's envelope is minted fresh at egress from the local roster.
//
// It is FALSE for a relay-ingested message durably recorded before RELAY-48 gave
// this bus somewhere to keep the attestation, which is the one case where the
// answer is a genuine, unrecoverable loss — and it is a loss confined to the
// ONWARD hop of that message.
func (m Message) HasOriginAttestation() bool { return !attestationIsZero(m.OriginAttestation) }

// validateOriginAttestation is the ONE definition of "this attestation is fit to
// be stored beside this sender", applied on the write path (WithRelayOrigin) and
// again on the recovery path (Decode) so a restart can never load state the write
// path would have refused to create.
func validateOriginAttestation(a attest.Attestation, sender string) error {
	if _, err := attest.Canonicalize(a); err != nil {
		return fmt.Errorf("%w: origin attestation: %s", ErrInvalidMessage, err)
	}
	if err := signing.ValidateSignature(a.Signature); err != nil {
		// The signature bytes are not echoed; the length is the whole fault.
		return fmt.Errorf("%w: origin attestation signature: %s", ErrInvalidMessage, err)
	}
	if a.AgentID != sender {
		// Both values have been through an id parse by the time this can fire —
		// Canonicalize validates the subject and the constructors validate the
		// sender — so both are bounded and safe to echo.
		return fmt.Errorf("%w: the origin attestation names subject %q but the message's sender is %q; the next hop verifies the attestation AGAINST the sender, so a mismatched pair could never be delivered anywhere", ErrInvalidMessage, a.AgentID, sender)
	}
	return nil
}

// attestationIsZero reports whether a carries nothing at all, which is what
// "this bus is the ORIGIN of this message" looks like: this bus mints no
// attestation for its own traffic, so absence is the ordinary case and is
// meaningful rather than an error.
//
// It enumerates every field of attest.Attestation deliberately. If that type ever
// gains one, this function is stale in the SAFE direction — a value carrying only
// the new field reads as PRESENT, is then put through validateOriginAttestation,
// and is refused loudly. The unsafe direction (a populated attestation read as
// absent, and silently dropped on the way to disk) is not reachable from here.
func attestationIsZero(a attest.Attestation) bool {
	return a.AgentID == "" &&
		len(a.MessagingPublicKey) == 0 &&
		a.KeyEpoch == 0 &&
		a.IssuedAtUnixMilli == 0 &&
		a.NotAfterUnixMilli == 0 &&
		len(a.Signature) == 0
}

// cloneAttestation snapshots both byte slices inside a value-typed attestation.
// Struct assignment alone still aliases them, which is the aliasing bug
// internal/relay's identically-named helper exists to prevent on the wire side;
// this is the durability side of the same fence.
func cloneAttestation(a attest.Attestation) attest.Attestation {
	out := a
	out.MessagingPublicKey = append(ed25519.PublicKey(nil), a.MessagingPublicKey...)
	out.Signature = append([]byte(nil), a.Signature...)
	return out
}

// VisibleTo reports whether agentID, enrolled at enrolledAt, is entitled to
// receive this message.
//
// THIS IS THE AUTHORIZATION BOUNDARY OF THE READ PATH. Every batch handed to a
// client — history and long-poll alike — is filtered through it with the
// AUTHENTICATED principal, never with an id taken from a cursor, a query
// parameter or a body. A cursor carries a POSITION and nothing else; it can
// never widen what its holder may see.
//
// # The enrolment epoch: you do not receive mail sent before you existed
//
// A message sent BEFORE enrolledAt is never delivered, whatever it is addressed
// to. This is not belt-and-braces; it closes a real hole that this epic opened,
// found by the security audit on 2026-08-02:
//
// Message records are durable and they name agent ids. Enrolment is NOT durable
// yet (AUTH-3), and the per-name suffix counter therefore restarts at 1 on
// every boot — so after a restart, anyone who can reach the unauthenticated
// /v1/enroll and guesses the name "alpha" is minted the id "<bus>.alpha-1"
// that the PREVIOUS alpha held, and could read a full retention window of that
// agent's direct messages. The bus cannot tell the two apart by id, because an
// id is exactly what is being reused.
//
// It CAN tell them apart by time. No legitimate agent needs traffic that
// predates its own enrolment, and after a restart every enrolment is newer than
// every recovered message, so the hole closes with no cost to a correct client.
//
// The rule stays right once AUTH-3 lands: a durable roster restores each
// agent's ORIGINAL enrolment instant, so a genuinely continuous agent keeps
// seeing everything sent since it enrolled. Nothing here has to be undone.
//
// A ZERO enrolledAt disables the check. That is for callers that legitimately
// have no roster — an operator dump, an audit tool — and must never be reached
// from a request path with a client's principal.
//
// The sender is deliberately excluded from its own message. An agent that
// broadcasts does not want its own traffic echoed back into the loop it is
// polling, and it already holds the message id from the send response. Stated
// in CONTRACTS-HTTP.md so it is a contract rather than an accident.
func (m Message) VisibleTo(agentID string, enrolledAt time.Time) bool {
	if agentID == "" || agentID == m.Sender {
		return false
	}
	if !enrolledAt.IsZero() && m.SentAt.Before(enrolledAt) {
		return false
	}
	if m.Broadcast {
		return true
	}
	for _, r := range m.Recipients {
		if r == agentID {
			return true
		}
	}
	return false
}

// Record is the JSON payload of the wal.Entry that makes a message durable.
//
// # This is the shape DUR-5 consumes
//
// Invariant 6 says the audit log records METADATA AND ROUTING INFO ONLY. Every
// field invariant 6 names is here as a top-level field, and the ONE field that
// must not reach the audit log — Body — is last and is the only one DUR-5 has
// to drop. DUR-5 therefore needs no format change on this side: it lifts
// message_id, seq, sender, recipients, broadcast, bus_path, sent_at, size and
// content_sha256 verbatim and writes them to its own append-only file.
//
// # Forward compatibility
//
// Decoding is deliberately NOT strict about unknown fields, which is the
// opposite of the request decoders in internal/httpapi and is a considered
// choice: request bodies are untrusted input where an unknown field is a
// client bug worth reporting, while THIS decoder reads records THIS SERVER
// WROTE, possibly by a newer build during a rolling restart. Refusing an
// unknown field here would turn a forward-compatible addition into a refusal
// to recover. That is what lets the CRYPTO epic add an encrypted-envelope
// descriptor without an on-disk break.
//
// # Message.Pos is DELIBERATELY ABSENT, and adding it would be a mistake
//
// The delivery position is the WAL commit index of the very entry this record
// is the body of, so writing it INTO that entry would record a fact the entry's
// own location already states — and would create a second copy free to disagree
// with the first. Decode therefore returns a Message with Pos == 0 and Hub.Apply
// stamps it from wal.Committed.CommitIndex, which is what makes the position
// identical on the live and the recovery paths without an on-disk format change.
type Record struct {
	V             int      `json:"v"`
	MessageID     string   `json:"message_id"`
	Seq           uint64   `json:"seq"`
	Sender        string   `json:"sender"`
	Broadcast     bool     `json:"broadcast"`
	Recipients    []string `json:"recipients,omitempty"`
	BusPath       []string `json:"bus_path,omitempty"`
	SentAtUnixNs  int64    `json:"sent_at_unix_ns"`
	Size          int      `json:"size"`
	ContentSHA256 string   `json:"content_sha256"`

	// OriginMessageID is the id the ORIGIN bus minted, present ONLY on a message
	// this bus ingested over a relay hop. See Message.OriginMessageID for what it
	// is (a CORRELATION key, inert in every ordering and retention decision).
	//
	// # RecordVersion STAYS AT 2, and no number was reserved
	//
	// It is an OPTIONAL added field, which is the case this record was shaped
	// for. RecordVersion's own doc: "Record is deliberately shaped so that the
	// CRYPTO epic and DUR-5 can ADD optional fields without a break … and an
	// added optional field does not move this number." The Record doc says
	// decoding is deliberately NOT strict about unknown fields, for exactly this
	// reason. So both directions of a rolling restart are already correct: an
	// OLD build reading a NEW record ignores the field, and a NEW build reading
	// an OLD record gets "" — which is not a loss of information but the RIGHT
	// answer, because a pre-relay bus originated every message it holds.
	//
	// # WHY IT MUST BE DURABLE AT ALL
	//
	// Its only consumer is relay.Forwarder.RecoverMessage, called from exactly
	// one place: Forwarder.Resume, which runs ONLY AFTER A RESTART. A correlation
	// field held in memory alone would therefore be empty at precisely the moment
	// — and the only moment — it is read. That is a trap, not an optimisation.
	//
	// This is the OPPOSITE case to Message.Pos, which is deliberately absent from
	// this record: the position is DERIVABLE from where the entry sits in the log,
	// so writing it down would be a second copy of a fact the log already states.
	// The origin's id is derivable from NOTHING on this bus — the local id, the
	// local sequence and the bus path all describe this bus's own view — so if it
	// is not written here it is gone.
	OriginMessageID string `json:"origin_message_id,omitempty"`

	// OriginAttestation is the ORIGIN bus's signed binding for Sender, present
	// ONLY on a message this bus ingested over a relay hop. See
	// Message.OriginAttestation for what it is and why it must be durable
	// (RELAY-48: without it a pending onward hop is destroyed at restart).
	//
	// # RecordVersion STAYS AT 2, AND NO NUMBER WAS RESERVED
	//
	// Same case, same reasoning as OriginMessageID above: an OPTIONAL added field
	// is what this record is shaped for, RecordVersion's own doc says an added
	// optional field does not move it, and Decode is deliberately non-strict about
	// unknown fields. Both directions of a rolling restart are therefore already
	// correct — an OLD build reading a NEW record ignores it, and a NEW build
	// reading an OLD record gets nil, which is not a loss but the right answer for
	// a bus whose every message it originated itself. (The decision to put it HERE
	// rather than on relay.OutboxRecord is recorded in DECISIONS.md, 2026-08-16:
	// the outbox record is per-HOP, so it would hold one copy per pending hop of a
	// fact that belongs to the MESSAGE — the same "second copy free to disagree"
	// argument that keeps Pos off this record.)
	//
	// # WHY A POINTER, WHEN Message HOLDS A VALUE
	//
	// encoding/json's omitempty does nothing for a struct, so a value here would
	// write a full skeleton of nulls and zeroes onto every locally-originated
	// message on the bus. The pointer is a JSON-layer concern ONLY and never
	// escapes: Record() takes the address of a copy, Decode dereferences into the
	// value on Message, and no caller is handed the pointer. attest.Attestation's
	// own doc requires a VALUE wherever it might be VERIFIED — a nil there is a
	// panic instead of a refusal — and that rule is honoured: nothing verifies a
	// Record.
	//
	// # SIZE
	//
	// It is bounded and small. AgentID is bounded by ids.ParseAgentID
	// (attest.Canonicalize applies it), the messaging key is exactly 32 bytes and
	// the signature exactly 64, both rendered as base64 by encoding/json, and the
	// remaining three fields are integers — under 300 bytes on disk in total, on
	// relay-ingested messages only. It does not scale with anything.
	OriginAttestation *attest.Attestation `json:"origin_attestation,omitempty"`

	// IdempotencyKey is durable so the applied-key memory is part of RECOVERED
	// STATE and not an in-memory cache (invariant 10).
	IdempotencyKey string `json:"idempotency_key"`

	// TimestampUnixMilli is the SENDER's clock — see Message.TimestampUnixMilli.
	// It is a separate durable field from sent_at because the two are different
	// facts and only this one is covered by the signature.
	TimestampUnixMilli int64 `json:"timestamp_ms"`

	// Signature is the sender's detached Ed25519 signature. encoding/json
	// renders []byte as standard base64, so it costs 88 bytes on disk and stays
	// readable with a JSON pretty-printer.
	//
	// It is deliberately NOT last: Body is the one field the audit log must
	// drop, and keeping it last keeps that rule "drop the final field" rather
	// than "drop the field in the middle". The signature is metadata and DUR-5
	// may carry it.
	Signature []byte `json:"signature"`

	// Body is LAST, and is the only field the audit log must not copy.
	// encoding/json renders []byte as standard base64, so the durable record
	// stays readable with a JSON pretty-printer and carries arbitrary bytes.
	Body []byte `json:"body"`
}

// ContentHash returns the hex SHA-256 of body. It is the one place the content
// hash is computed, so the write path and the recovery check can never disagree
// about what "the hash of this body" means.
func ContentHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// LocalBusPath is the bus path of a message this bus accepted DIRECTLY from one
// of its own clients: this bus, and nothing else.
//
// It is a function rather than an inline literal at each site so that "the path
// of a locally-accepted message" has one definition. Every caller gets a FRESH
// slice, which matters because the value ends up on a Message that outlives the
// call and on an audit record that is about to be written.
func LocalBusPath(busID string) []string { return []string{busID} }

// NewMessage builds a validated Message for a send this bus accepted DIRECTLY
// from a local client. Its bus path is LocalBusPath(busID) — one hop, this bus.
//
// It is the constructor for the local write path; NewMessageWithBusPath below is
// the same constructor for a message INGESTED FROM A PEER, which arrives with
// the hops it has already traversed. Between them they are the ONLY constructors:
// the fields are interdependent (id derives from seq, hash derives from body) and
// building a Message by hand is how they drift apart.
//
// timestampUnixMilli and signature are the two SENDER-supplied fields, and both
// are MANDATORY. That is SIGN-6: there is no unsigned message type, so a
// constructor that would accept a zero timestamp or an absent signature would be
// the hole the whole epic exists to close — an attacker (or a lazy client) that
// simply omits the field would get a durable, delivered, un-verifiable message.
// The bound on the signature is a LENGTH and nothing more; see Message.Signature
// for why this package must not verify.
func NewMessage(busID, sender string, broadcast bool, recipients []string, seq uint64, sentAt time.Time, body []byte, idempotencyKey string, timestampUnixMilli int64, signature []byte) (Message, error) {
	// The path is built HERE and is by construction valid, so while the bus-path
	// checks below still RUN on this entry point, none of them can FAIL on it: a
	// local send is refused for exactly the reasons it was refused before this
	// function had a sibling.
	return NewMessageWithBusPath(busID, sender, broadcast, recipients, seq, sentAt, body, idempotencyKey, timestampUnixMilli, signature, LocalBusPath(busID))
}

// NewMessageWithBusPath is NewMessage for a message that reaches this bus over a
// RELAY HOP, carrying the ordered list of buses it has already traversed.
//
// # Why this exists at all
//
// Invariant 6 says the audit log records "the bus path traversed", and that is
// the entire reason a relay hop is auditable. Until RELAY-11 the path was
// hard-coded to []string{busID} inside the one constructor, so a multi-hop path
// was UNWRITABLE: wal.AuditRecord.BusPath existed and was validated, and nothing
// could ever put more than one hop in it.
//
// # busPath is UNTRUSTED INPUT, and is validated as such
//
// It arrives from a PEER BUS over the network, so every hop is checked with the
// same ids.ValidateBusID the recovery decoder applies, and the list is bounded by
// MaxBusPath. An unvalidated hop list would be attacker-chosen content echoed
// verbatim to every client that reads the message, written into an APPEND-ONLY
// trail that cannot be edited afterwards.
//
// # THE LAST HOP MUST BE THIS BUS, and the caller appends it
//
// A path is APPEND-ONLY and ORIGIN-FIRST: [origin, …, this bus]. The ingesting
// caller appends its own bus id before calling, and this constructor refuses a
// path that does not end in busID.
//
// That is deliberately checkable rather than done for the caller, because the
// path AS RECEIVED ON THE WIRE has a different last hop — the PEER that sent it —
// and internal/relay must check THAT against the authenticated connection
// (internal/relay/doc.go, gap 6) before appending anything. Doing the append here
// would erase the distinction between "the path a peer asserted" and "the path
// this bus is willing to swear to", and the first is exactly what must not be
// written to the trail unexamined. A refusal here is loud; a trail at bus C that
// never names C would be silently wrong for ever.
//
// It also keeps ONE meaning for the field on disk. A locally-accepted message has
// always recorded [busID] — the recording bus, last and only — so "the final hop
// is the bus whose record this is" holds for every record in the trail rather
// than for some of them, and a reader never has to know how a message arrived to
// know whether this bus is on its path.
//
// # THE TRAP FOR THE RELAY INGEST, stated because the two conventions differ
//
// internal/relay stamps its own hop at EGRESS, not at ingress: RelayedMessage
// carries the path AS RECEIVED (relay.CheckIncomingPath REFUSES one that already
// contains this bus), and relay.Forward calls relay.AppendHop when it re-relays
// onward. PROTOCOL.md §10's "the last element is always whichever bus most
// recently forwarded it" describes THAT wire value — §10 is "Loop prevention and
// the relay envelope"; §8 is the canonical signing format and says nothing about
// this.
//
// So an ingest holds TWO different paths and must not conflate them:
//
//	// ends at THIS bus. A FRESH slice, never append(m.BusPath, …): the
//	// received slice may have spare capacity, and appending into it would
//	// rewrite a path another outbound forward is about to read.
//	stored here = append(append([]string(nil), m.BusPath...), localBusID)
//
//	// the received path, UNCHANGED — what Forward must be given
//	m.BusPath
//
// Handing the stored path to Forward is refused as relay.ErrRelayLoop, and
// handing the received path to this constructor is refused here. Both directions
// fail closed and loudly, which is the point of stating it rather than papering
// over it with an append nobody sees.
//
// # What is deliberately NOT checked here
//
// LOOP PREVENTION. A path that already contains busID is a routing loop, and
// refusing it belongs to the relay's ingest decision — together with the split
// horizon and the "forward only on a new acceptance" rule — not to a constructor
// that cannot see the topology. This function's job is that the path is
// well-formed and names this bus last; whether the message should have arrived at
// all is answered before it gets here.
func NewMessageWithBusPath(busID, sender string, broadcast bool, recipients []string, seq uint64, sentAt time.Time, body []byte, idempotencyKey string, timestampUnixMilli int64, signature []byte, busPath []string) (Message, error) {
	id, err := ids.MessageID(busID, seq)
	if err != nil {
		return Message{}, fmt.Errorf("%w: %s", ErrInvalidMessage, err)
	}
	if len(body) > MaxBodyBytes {
		return Message{}, fmt.Errorf("%w: body is %d bytes, the limit is %d", ErrInvalidMessage, len(body), MaxBodyBytes)
	}
	if len(recipients) > MaxRecipients {
		return Message{}, fmt.Errorf("%w: %d recipients, the limit is %d", ErrInvalidMessage, len(recipients), MaxRecipients)
	}
	// 0 means "unset", exactly as it does for a sequence and for
	// signing.Message.TimestampUnixMilli — and a negative value is a pre-1970
	// clock, which the canonical format can ENCODE but which validate() there
	// refuses, so accepting one here would build a message that can never be
	// canonicalized and therefore never verified.
	if timestampUnixMilli <= 0 {
		return Message{}, fmt.Errorf("%w: timestamp %d is not a positive Unix millisecond value; 0 means \"unset\" and every message carries the sender's clock (SIGN-6)", ErrInvalidMessage, timestampUnixMilli)
	}
	if len(signature) != signing.SignatureSize {
		// The signature itself is not echoed: it is attacker-chosen bytes headed
		// for a log line, and its LENGTH is the whole of what was wrong.
		return Message{}, fmt.Errorf("%w: signature is %d bytes, but every message carries a detached Ed25519 signature of exactly %d (SIGN-6)", ErrInvalidMessage, len(signature), signing.SignatureSize)
	}
	// THE BUS PATH, checked LAST of the input checks so that every error a local
	// send could produce before RELAY-11 is still produced with the same message
	// and in the same order. Nothing below can FAIL for NewMessage, which builds
	// the path itself — the checks run there, they just cannot fire.
	//
	// These are the SAME bounds Decode applies to a path read off disk, for the
	// same reason: both are boundaries for a list this process did not build.
	if len(busPath) == 0 {
		// An empty path is NOT quietly defaulted to this bus. That would turn a
		// relay ingest that lost its path into a durable, append-only record
		// asserting the message ORIGINATED here — a provenance claim nobody made,
		// indistinguishable afterwards from a genuine local send.
		return Message{}, fmt.Errorf("%w: the bus path is empty; a message has always been accepted by at least one bus, and a locally-accepted message is built with LocalBusPath", ErrInvalidMessage)
	}
	if len(busPath) > MaxBusPath {
		return Message{}, fmt.Errorf("%w: bus path has %d hops, the limit is %d", ErrInvalidMessage, len(busPath), MaxBusPath)
	}
	for i, b := range busPath {
		if err := ids.ValidateBusID(b); err != nil {
			return Message{}, fmt.Errorf("%w: bus path hop %d: %s", ErrInvalidMessage, i, err)
		}
	}
	if last := busPath[len(busPath)-1]; last != busID {
		// The offending hop IS echoed, and safely: it has just passed
		// ids.ValidateBusID, so it is at most 64 characters of [A-Za-z0-9_-].
		return Message{}, fmt.Errorf("%w: the bus path ends at %q, but this bus is %q; the path is append-only and origin-first, so the ingesting bus appends itself as the final hop before building the message", ErrInvalidMessage, last, busID)
	}
	m := Message{
		Seq:       seq,
		ID:        id,
		Sender:    sender,
		Broadcast: broadcast,
		// Copied, not aliased: the caller may still hold the slice, and a
		// mutation after acceptance would silently re-address a message that is
		// already durable.
		Recipients: append([]string(nil), recipients...),
		// Copied for the same reason the recipient list is, and with one more: this
		// slice may be the peer-supplied one the relay is still holding, and a
		// mutation after acceptance would rewrite the provenance of a message that
		// is already durable.
		BusPath:        append([]string(nil), busPath...),
		SentAt:         sentAt,
		Body:           append([]byte(nil), body...),
		ContentSHA256:  ContentHash(body),
		IdempotencyKey: idempotencyKey,

		TimestampUnixMilli: timestampUnixMilli,
		// Copied for the same reason the body and the recipient list are: the
		// caller still holds these bytes, and a signature mutated after
		// acceptance would no longer be the one the sender produced over a
		// message that is already durable.
		Signature: append([]byte(nil), signature...),
	}
	return m, nil
}

// SigningMessage returns the signing.Message whose canonicalization this
// message's Signature covers. It is the ONE place the mapping from a stored
// message to its signed bytes is written down.
//
// Everything about verification depends on this mapping being identical on both
// sides, and the failure mode when it is not is silent: a verifier that
// reconstructs the bytes slightly differently — one extra field, a different
// timestamp source, the bus path included — computes a signature check that
// simply returns false, for every message, for ever, with no test able to tell
// that from a genuine forgery. So there is exactly one function, and any code
// that needs the signed bytes calls it rather than assembling its own copy.
//
// NOTE WHAT IS ABSENT, and do not "complete" it: SentAt (this bus's clock),
// BusPath (rewritten by every relay hop, deliberately uncovered — SIGN-1),
// ContentSHA256 (derived from Body, which is already covered), IdempotencyKey
// and Broadcast. A broadcast has an EMPTY Recipients, which
// signing.Canonicalize rejects outright — "an empty recipient set would sign an
// audience of nobody" — which is exactly why /v1/broadcast is refused today
// rather than accepting unsigned traffic. This method still returns the
// broadcast's mapping unchanged; the refusal belongs at the route, not here,
// and SIGN-3 will settle what a broadcast's canonical audience is.
func (m Message) SigningMessage() signing.Message {
	return signing.Message{
		MessageID:          m.ID,
		Sequence:           m.Seq,
		Sender:             m.Sender,
		Recipients:         m.Recipients,
		TimestampUnixMilli: m.TimestampUnixMilli,
		Body:               m.Body,
	}
}

// Record renders m as the durable JSON record.
func (m Message) Record() Record {
	return Record{
		V:             RecordVersion,
		MessageID:     m.ID,
		Seq:           m.Seq,
		Sender:        m.Sender,
		Broadcast:     m.Broadcast,
		Recipients:    m.Recipients,
		BusPath:       m.BusPath,
		SentAtUnixNs:  m.SentAt.UTC().UnixNano(),
		Size:          len(m.Body),
		ContentSHA256: m.ContentSHA256,

		// Empty on a locally-originated message, where ID already IS the origin
		// id; the `omitempty` tag keeps it off disk entirely in that case.
		OriginMessageID: m.OriginMessageID,

		// nil on a locally-originated message: this bus mints no attestation for
		// its own traffic, so absence is the ordinary case rather than a fault.
		// The pointer addresses a COPY (cloneAttestation), so the record cannot
		// alias — and therefore cannot be mutated through — the durable message's
		// key and signature bytes.
		OriginAttestation: originAttestationRecord(m.OriginAttestation),

		IdempotencyKey: m.IdempotencyKey,

		TimestampUnixMilli: m.TimestampUnixMilli,
		Signature:          m.Signature,

		Body: m.Body,
	}
}

// decodedOriginAttestation is originAttestationRecord's inverse: it turns the
// record's optional pointer back into the VALUE Message holds, copying the byte
// slices so a recovered message never aliases the decoded JSON.
//
// The value form is what attest.Attestation's own doc requires of anything that
// might be verified: a nil pointer reaching a verification path is a panic where
// a zero value is a refusal.
func decodedOriginAttestation(a *attest.Attestation) attest.Attestation {
	if a == nil {
		return attest.Attestation{}
	}
	return cloneAttestation(*a)
}

// originAttestationRecord renders m's origin attestation for the durable record:
// nil when there is none, and otherwise a pointer to a fresh COPY so the record
// cannot alias the message's key and signature bytes.
//
// It is a function rather than an inline conditional so that "how an absent
// attestation is written" has ONE definition. nil is what makes `omitempty` keep
// the key off disk entirely on every locally-originated message, which is most of
// them — a struct value would write a skeleton of nulls and zeroes instead.
func originAttestationRecord(a attest.Attestation) *attest.Attestation {
	if attestationIsZero(a) {
		return nil
	}
	out := cloneAttestation(a)
	return &out
}

// Encode marshals m for the durable write path.
func (m Message) Encode() (json.RawMessage, error) {
	b, err := json.Marshal(m.Record())
	if err != nil {
		return nil, fmt.Errorf("store: encoding message %s: %w", m.ID, err)
	}
	return json.RawMessage(b), nil
}

// Decode turns a durable record back into a Message, VALIDATING it.
//
// This runs on the recovery path, where the input is bytes off a disk that may
// have been written by an older build, corrupted by media, or — once the bus
// federates — handed over by a peer. Every cross-field relationship is
// re-checked here rather than assumed, because a record that passes the WAL's
// framing MAC is proof that the BYTES are the bytes we wrote, not that their
// CONTENT is coherent.
//
// The content hash check is the load-bearing one: it is what makes a body that
// was silently altered a recovery error instead of a message this bus would go
// on to serve and, worse, whose hash it would keep asserting in the audit log.
func Decode(raw json.RawMessage) (Message, error) {
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Message{}, fmt.Errorf("%w: %s", ErrInvalidMessage, err)
	}
	if rec.V != RecordVersion {
		// EXACT match, in BOTH directions, and each direction means something
		// different to the operator:
		//
		//   - A record from a FUTURE build is refused rather than guessed at:
		//     the operator downgraded, and the fix is to run the newer binary.
		//   - A record from an OLDER build (v1) is refused because it carries no
		//     signature and no sender timestamp, and SIGN-6 admits no unsigned
		//     message. The fix is NOT to run an older binary — it is to accept
		//     that the pre-SIGN-6 history is gone. See RecordVersion for why
		//     there is no migration.
		//
		// Either way the caller DISCARDS the record and says so loudly
		// (Hub.Apply); invariant 6 sanctions the discard, silent discard is the
		// defect.
		return Message{}, fmt.Errorf("%w: record schema version %d, this build understands %d", ErrInvalidMessage, rec.V, RecordVersion)
	}
	if rec.Seq == 0 {
		return Message{}, fmt.Errorf("%w: sequence 0 is never allocated", ErrInvalidMessage)
	}
	busID, seq, err := ids.ParseMessageID(rec.MessageID)
	if err != nil {
		return Message{}, fmt.Errorf("%w: %s", ErrInvalidMessage, err)
	}
	if seq != rec.Seq {
		return Message{}, fmt.Errorf("%w: message id %q carries sequence %d but the record says %d", ErrInvalidMessage, rec.MessageID, seq, rec.Seq)
	}
	if _, _, _, err := ids.ParseAgentID(rec.Sender); err != nil {
		return Message{}, fmt.Errorf("%w: sender: %s", ErrInvalidMessage, err)
	}
	if len(rec.Recipients) > MaxRecipients {
		return Message{}, fmt.Errorf("%w: %d recipients, the limit is %d", ErrInvalidMessage, len(rec.Recipients), MaxRecipients)
	}
	for i, r := range rec.Recipients {
		if _, _, _, err := ids.ParseAgentID(r); err != nil {
			return Message{}, fmt.Errorf("%w: recipient %d: %s", ErrInvalidMessage, i, err)
		}
	}
	if rec.Broadcast && len(rec.Recipients) != 0 {
		return Message{}, fmt.Errorf("%w: a broadcast carries no recipient list, but this record has %d", ErrInvalidMessage, len(rec.Recipients))
	}
	if !rec.Broadcast && len(rec.Recipients) == 0 {
		return Message{}, fmt.Errorf("%w: a directed message must name at least one recipient", ErrInvalidMessage)
	}
	if len(rec.Body) > MaxBodyBytes {
		return Message{}, fmt.Errorf("%w: body is %d bytes, the limit is %d", ErrInvalidMessage, len(rec.Body), MaxBodyBytes)
	}
	if rec.Size != len(rec.Body) {
		return Message{}, fmt.Errorf("%w: record declares a %d-byte body but carries %d", ErrInvalidMessage, rec.Size, len(rec.Body))
	}
	if got := ContentHash(rec.Body); got != rec.ContentSHA256 {
		// The body is NOT echoed: it may be a megabyte, and once the CRYPTO
		// epic lands it is ciphertext nobody here can read anyway.
		return Message{}, fmt.Errorf("%w: content hash mismatch for %s: the record asserts %q, the body hashes to %q", ErrInvalidMessage, rec.MessageID, rec.ContentSHA256, got)
	}
	// The idempotency key is bounded HERE as well as at the handler, because
	// this decoder is the boundary for records this process did not validate:
	// a file written by an older build, damaged media, and — once the bus
	// federates — a record handed over by a peer. The key is reflected into
	// the server log, so an unbounded one off disk is an unbounded log line.
	if len(rec.IdempotencyKey) > MaxIdempotencyKeyLen {
		return Message{}, fmt.Errorf("%w: idempotency key is %d bytes, the limit is %d; the key is not echoed here because it is oversized", ErrInvalidMessage, len(rec.IdempotencyKey), MaxIdempotencyKeyLen)
	}
	// The ORIGIN MESSAGE ID is validated with EXACTLY the rule
	// Message.WithOriginMessageID applies on the write path, and for the reason
	// every other check here exists: this decoder is the boundary for bytes THIS
	// PROCESS DID NOT VALIDATE — a file written by another build, damaged media,
	// or a record handed over by a peer. Absent is legal and means "this bus is
	// the origin"; present and naming THIS bus is not, because a message this bus
	// minted is its own origin and a record asserting otherwise is a second copy
	// of one fact, free to disagree with rec.MessageID. That refusal also keeps
	// Store.ByOriginMessageID's fallback sound after a restart.
	//
	// Nothing beyond what ids.ParseMessageID already bounds is echoed: the parse
	// error carries its own (bounded, or elided when oversized) rendering.
	if rec.OriginMessageID != "" {
		originBus, _, err := ids.ParseMessageID(rec.OriginMessageID)
		if err != nil {
			return Message{}, fmt.Errorf("%w: origin message id: %s", ErrInvalidMessage, err)
		}
		if originBus == busID {
			return Message{}, fmt.Errorf("%w: record for %s carries origin message id %q, which names the SAME bus (%q); a message this bus minted is its own origin and the field is written only for a message ingested over a relay hop", ErrInvalidMessage, rec.MessageID, rec.OriginMessageID, busID)
		}
	}
	// THE ORIGIN ATTESTATION, when the record carries one, is validated with
	// EXACTLY the rule WithRelayOrigin applies on the write path — one function,
	// validateOriginAttestation, called from both — so a restart can never load
	// state the write path would have refused to create.
	//
	// AN ATTESTATION WITHOUT AN ORIGIN MESSAGE ID IS REFUSED. The two facts are
	// written together by the one setter there is, so a record carrying only the
	// second is a claim that a message THIS BUS ORIGINATED needs somebody else's
	// bus to vouch for its sender, which is incoherent: this bus mints no
	// attestation for its own traffic and never adopts a peer's for it.
	//
	// THE CONVERSE IS ACCEPTED, and deliberately: an origin id with NO attestation
	// is what every record written before this field existed would look like, and
	// what WithOriginMessageID alone still produces. It decodes, it serves, and it
	// is delivered to local recipients exactly as before — only the ONWARD hop is
	// unrebuildable, and that is settled loudly, one job at a time, by the
	// forwarder's recovery seam. Refusing the record here instead would discard a
	// whole durable MESSAGE to protect one hop of it, which is the wrong blast
	// radius (invariant 6 sanctions a discard; it does not ask for a wider one
	// than the fault).
	if rec.OriginAttestation != nil {
		if rec.OriginMessageID == "" {
			return Message{}, fmt.Errorf("%w: record for %s carries an ORIGIN ATTESTATION but no origin message id, so it claims this bus originated a message whose sender another bus vouches for; the two are written together or not at all", ErrInvalidMessage, rec.MessageID)
		}
		if err := validateOriginAttestation(*rec.OriginAttestation, rec.Sender); err != nil {
			return Message{}, fmt.Errorf("record for %s: %w", rec.MessageID, err)
		}
	}
	// BusPath is VALIDATED, not merely carried, for the same reason and one
	// more: it is echoed verbatim to every client that reads the message. An
	// unvalidated hop list off disk is attacker-chosen content on a response
	// body, and it is the field the RELAY epic will make loop-prevention
	// decisions from.
	if len(rec.BusPath) > MaxBusPath {
		return Message{}, fmt.Errorf("%w: bus path has %d hops, the limit is %d", ErrInvalidMessage, len(rec.BusPath), MaxBusPath)
	}
	for i, b := range rec.BusPath {
		if err := ids.ValidateBusID(b); err != nil {
			return Message{}, fmt.Errorf("%w: bus path hop %d: %s", ErrInvalidMessage, i, err)
		}
	}
	// The SIGN-6 pair, re-checked here with the SAME bounds NewMessage applies,
	// because this decoder is the boundary for bytes THIS PROCESS DID NOT
	// VALIDATE: a file written by another build, damaged media, and — once the
	// bus federates — a record handed over by a peer. A record that reached disk
	// without them is not an older shape of a message, it is a message this
	// build will not claim was signed, and serving it would put an unsigned
	// message on a read path whose whole contract is that every message carries
	// a signature to verify.
	//
	// The LENGTH is all that is checked. Verifying here is impossible and would
	// be wrong even if it were possible: this bus does not hold the sender's
	// messaging key (Message.Signature).
	if rec.TimestampUnixMilli <= 0 {
		return Message{}, fmt.Errorf("%w: record for %s carries timestamp %d; 0 means \"unset\" and every message carries the sender's clock (SIGN-6)", ErrInvalidMessage, rec.MessageID, rec.TimestampUnixMilli)
	}
	if len(rec.Signature) != signing.SignatureSize {
		// The signature bytes are NOT echoed: they are off untrusted media on
		// their way to a log line, and the length is the whole of the fault.
		return Message{}, fmt.Errorf("%w: record for %s carries a %d-byte signature, but every message carries a detached Ed25519 signature of exactly %d (SIGN-6)", ErrInvalidMessage, rec.MessageID, len(rec.Signature), signing.SignatureSize)
	}
	busPath := rec.BusPath
	if len(busPath) == 0 {
		// A COMPATIBILITY DEFAULT, and the one place an absent path is read as a
		// local one. It is not the same judgement NewMessageWithBusPath makes when
		// it REFUSES an empty path: there the caller is a live ingest that has lost
		// its path and must be told, here the record is already durable and the
		// only question is what to serve for it. Written through LocalBusPath so
		// there is one definition of "the path of a locally-accepted message".
		busPath = LocalBusPath(busID)
	}
	return Message{
		Seq:           rec.Seq,
		ID:            rec.MessageID,
		Sender:        rec.Sender,
		Broadcast:     rec.Broadcast,
		Recipients:    rec.Recipients,
		BusPath:       busPath,
		SentAt:        time.Unix(0, rec.SentAtUnixNs).UTC(),
		Body:          rec.Body,
		ContentSHA256: rec.ContentSHA256,

		// Empty for a locally-originated message and for every record written
		// before this field existed; both mean "this bus is the origin", which is
		// what OriginID() reads it as.
		OriginMessageID: rec.OriginMessageID,

		// ZERO when the record carries none — a locally-originated message, or one
		// written before this field existed. The value is COPIED out of the record
		// so the recovered message does not alias the decoded JSON's buffers.
		OriginAttestation: decodedOriginAttestation(rec.OriginAttestation),

		IdempotencyKey: rec.IdempotencyKey,

		TimestampUnixMilli: rec.TimestampUnixMilli,
		Signature:          rec.Signature,
	}, nil
}
