package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
		V:              RecordVersion,
		MessageID:      m.ID,
		Seq:            m.Seq,
		Sender:         m.Sender,
		Broadcast:      m.Broadcast,
		Recipients:     m.Recipients,
		BusPath:        m.BusPath,
		SentAtUnixNs:   m.SentAt.UTC().UnixNano(),
		Size:           len(m.Body),
		ContentSHA256:  m.ContentSHA256,
		IdempotencyKey: m.IdempotencyKey,

		TimestampUnixMilli: m.TimestampUnixMilli,
		Signature:          m.Signature,

		Body: m.Body,
	}
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
		Seq:            rec.Seq,
		ID:             rec.MessageID,
		Sender:         rec.Sender,
		Broadcast:      rec.Broadcast,
		Recipients:     rec.Recipients,
		BusPath:        busPath,
		SentAt:         time.Unix(0, rec.SentAtUnixNs).UTC(),
		Body:           rec.Body,
		ContentSHA256:  rec.ContentSHA256,
		IdempotencyKey: rec.IdempotencyKey,

		TimestampUnixMilli: rec.TimestampUnixMilli,
		Signature:          rec.Signature,
	}, nil
}
