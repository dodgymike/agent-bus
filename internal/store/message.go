package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
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
// Bumping it is a last resort. Record is deliberately shaped so that the CRYPTO
// epic and DUR-5 can ADD optional fields without a break — see the Record doc —
// and an added optional field does not move this number.
const RecordVersion = 1

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

	// ErrOutOfOrder reports an Append whose sequence is not strictly greater
	// than the sequence already at the head of the store.
	ErrOutOfOrder = errors.New("store: message sequence is not strictly increasing")
)

// Message is one message in the serving copy.
//
// Every field is SERVER-DERIVED. Sender is the authenticated principal, never a
// value from the request body; Seq and ID are minted by the server (invariant
// 1); SentAt is the server's clock. A client supplies exactly two things — the
// body and, for a directed message, the recipient — and both are validated
// before they reach here.
type Message struct {
	// Seq is the server-minted sequence number. It is the total order of the
	// bus and the value a cursor points at.
	Seq uint64

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

// NewMessage builds a validated Message from server-derived parts. It is the
// ONLY constructor: the fields are interdependent (id derives from seq, hash
// derives from body) and building one by hand is how they drift apart.
func NewMessage(busID, sender string, broadcast bool, recipients []string, seq uint64, sentAt time.Time, body []byte, idempotencyKey string) (Message, error) {
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
	m := Message{
		Seq:       seq,
		ID:        id,
		Sender:    sender,
		Broadcast: broadcast,
		// Copied, not aliased: the caller may still hold the slice, and a
		// mutation after acceptance would silently re-address a message that is
		// already durable.
		Recipients:     append([]string(nil), recipients...),
		BusPath:        []string{busID},
		SentAt:         sentAt,
		Body:           append([]byte(nil), body...),
		ContentSHA256:  ContentHash(body),
		IdempotencyKey: idempotencyKey,
	}
	return m, nil
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
		Body:           m.Body,
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
		// A record from a FUTURE build is refused rather than guessed at. It is
		// a distinct message from "malformed" because the remedy is different:
		// the operator downgraded, and the fix is to run the newer binary.
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
	busPath := rec.BusPath
	if len(busPath) == 0 {
		busPath = []string{busID}
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
	}, nil
}
