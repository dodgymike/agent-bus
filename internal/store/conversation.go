package store

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dodgymike/agent-bus/internal/ids"
)

// ConversationRecordKind is the wal.Entry.Kind discriminator for a durable
// conversation record (CONV-RECORD).
//
// It is the EXACT reserved string "conversation": the `wal-entry-kind` namespace
// holds value 3 = "conversation" (reserved 2026-08-15, spec-keeper), alongside
// 1 = "message", 2 = "seqfloor" and 4 = "convmember". Entry.Kind is a FREE-FORM
// application discriminator that sits INSIDE the prepare payload, above the
// framing layer — it is not a numbered frame type — so NO `record-type`
// reservation is needed for it and internal/wal/format.go's Type enum is
// untouched. A business record rides inside the EXISTING TypePrepare/TypeCommit
// WAL frames exactly as "message" and "seqfloor" do (DECISIONS.md 2026-08-29,
// CONTRACTS-ONDISK.md).
const ConversationRecordKind = "conversation"

// conversationRecordVersion is the schema version of the durable conversation
// record. It is written and CHECKED on decode: a record from a future version is
// REFUSED rather than read with today's field meanings, the same posture
// ack.recordVersion and wal.FormatVersion take. It describes bytes on OUR disk,
// not a relay wire.
const conversationRecordVersion = 1

// MaxConversationNameBytes is the hard bound on the optional conversation NAME
// (CONV-NAME-INV6, DECISIONS.md 2026-08-29). The ruling is EARNED ENTIRELY BY
// THE BOUND: the name is metadata iff it is <= 128 bytes and single-line
// printable UTF-8; anything larger or with control characters is a body wearing
// metadata's clothes and is REFUSED. 128 bytes is three orders of magnitude
// below store.MaxBodyBytes (65536), large enough for a human-meaningful
// multi-word UTF-8 label and far too small to carry a message. The bound is on
// BYTES, not runes: it is the on-disk footprint that matters for the log and it
// removes rune-counting ambiguity across encodings.
const MaxConversationNameBytes = 128

// MaxConversationRecipients bounds the recipient list of one conversation. It
// reuses MaxRecipients (64) because a conversation's audience is the same class
// of routing metadata as a directed message's, and the bound exists for the same
// reason: DecodeConversationRecord reads whatever the FILE holds, not whatever a
// handler validated, so an unbounded list on the disk-decode path is unbounded
// memory on the startup path.
const MaxConversationRecipients = MaxRecipients

// MaxConversationIDLen is the longest a well-formed conversation id can be:
// the 64 bytes BusIDPattern allows a bus id, the '.' qualification separator
// (invariant 2), and the 36 bytes of a canonical UUIDv4. Anything longer cannot
// be valid whatever it contains, so the length is checked BEFORE the parse — the
// bound-before-quote discipline the whole durable layer uses so a hostile or
// damaged record cannot choose how much text a parse error quotes back.
const MaxConversationIDLen = 64 + len(".") + 36

// ErrInvalidConversation is a conversation record this bus refuses to write or
// to read back. It is the single sentinel for every validation failure so a
// caller can classify with errors.Is without depending on the exact wording.
var ErrInvalidConversation = errors.New("store: invalid conversation record")

// ConversationRecord is ONE durable conversation: a server-minted routing and
// addressing object (DECISIONS.md 2026-08-29, rulings 2 and 3).
//
// # IT CARRIES METADATA AND ROUTING ONLY (invariant 6)
//
// id, creator, an optional bounded name, the recipient list and a created-at
// timestamp — all bounded, structural, identity/routing-bearing fields. It holds
// NO message content of any kind. The name is admitted to the log ONLY because it
// is hard-capped and single-line printable (see MaxConversationNameBytes); a
// larger or control-bearing name is refused and never reaches the log.
//
// # THERE IS DELIBERATELY NO Pos FIELD
//
// store.Message.Pos is deliberately absent from the durable Message record for
// the reason message.go:437 gives — a second copy of a value is free to disagree
// with the first — and the same reasoning binds this record. A conversation's
// delivery position, if it ever needs one, is the wal.Committed.CommitIndex of
// the record that made it durable, derived at recovery, never stored.
type ConversationRecord struct {
	// ID is the server-minted identity, "<bus-id>.<uuid-v4>" (ruling 2). The whole
	// string is minted server-side: the prefix is this bus's own id, the suffix a
	// server-generated UUIDv4; no byte of it is client input (invariant 1). It is
	// never reused, including across restarts.
	ID string

	// Creator is the fully-qualified "<bus-id>.<agent-id>" of the agent that
	// created the conversation (invariant 2). Never shortened: the namespacing is
	// what makes cross-bus attribution unambiguous.
	Creator string

	// Name is the OPTIONAL, bounded, single-line label (CONV-NAME-INV6). Empty is
	// permitted. When present it satisfies MaxConversationNameBytes and the charset
	// rule, enforced on BOTH the construction path (Encode) and the disk-decode
	// path (DecodeConversationRecord); a name that exceeds the bound or carries a
	// disallowed codepoint is REFUSED, never truncated.
	Name string

	// Recipients is the conversation's membership: 1..MaxConversationRecipients
	// fully-qualified agent ids (invariant 2), with no duplicates. It is routing
	// metadata, not content.
	Recipients []string

	// CreatedAt is when this bus minted the conversation, by this bus's clock. It
	// is stored and read back in RFC3339Nano UTC.
	CreatedAt time.Time
}

// conversationRecordJSON is the durable shape: compact, no HTML escaping,
// omitempty on the optional name, the timestamp RFC3339Nano in UTC — so an
// operator can read a record straight out of the WAL with `head -c` and a JSON
// pretty-printer.
type conversationRecordJSON struct {
	Version    int      `json:"record_version"`
	ID         string   `json:"conversation_id"`
	Creator    string   `json:"creator"`
	Name       string   `json:"name,omitempty"`
	Recipients []string `json:"recipients"`
	CreatedAt  string   `json:"created_at"`
}

// NewConversationID mints a fresh conversation id "<busID>.<uuid-v4>" (ruling 2).
//
// busID is this bus's own server-minted id and is validated so a malformed one
// can never produce an unparseable conversation id. The uuid is a crypto/rand
// backed RFC 4122 UUIDv4 (see newUUIDv4): stdlib only, no third-party dependency
// (invariant 8), and unguessable and independent of every other minted sequence.
func NewConversationID(busID string) (string, error) {
	if err := ids.ValidateBusID(busID); err != nil {
		return "", fmt.Errorf("%w: minting a conversation id: %v", ErrInvalidConversation, err)
	}
	u, err := newUUIDv4()
	if err != nil {
		return "", fmt.Errorf("%w: minting a conversation id: %v", ErrInvalidConversation, err)
	}
	return busID + "." + u, nil
}

// newUUIDv4 returns a canonical RFC 4122 version-4 UUID string.
//
// It reads 16 bytes from crypto/rand and sets the version (0x40 in octet 6) and
// variant (0x80 in octet 8) bits per RFC 4122 §4.4, then renders the canonical
// 8-4-4-4-12 lowercase-hex form. This is NOT a cryptographic construction — it
// is a well-known layout over an audited randomness source — so invariant 9 is
// not engaged; it is exactly the stdlib-only recipe CONV-RECORD's brief
// prescribes rather than pulling in a uuid dependency (invariant 8).
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("reading 16 random bytes for a uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xxxxxx (RFC 4122)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// validateConversationID bounds and parses a conversation id "<bus-id>.<uuid-v4>"
// (ruling 2). It is applied on the construction path AND on the disk-decode path
// so a record read back from a tampered log with a malformed id is refused, not
// trusted (invariant 1: a client-supplied id is never trusted; a decoded id is
// input to be validated).
//
// The length is checked FIRST so a hostile or damaged record cannot choose how
// much text a parse error quotes back. The bus half must satisfy BusIDPattern
// (which excludes '.'), so the FIRST '.' unambiguously separates the bus id from
// the uuid.
func validateConversationID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: conversation_id is required", ErrInvalidConversation)
	}
	if len(id) > MaxConversationIDLen {
		return fmt.Errorf("%w: conversation_id is %d bytes, over the %d-byte limit; it is not echoed here because it is oversized", ErrInvalidConversation, len(id), MaxConversationIDLen)
	}
	busID, uuid, ok := strings.Cut(id, ".")
	if !ok {
		return fmt.Errorf("%w: conversation_id %q is not \"<bus-id>.<uuid-v4>\": no '.' separator (invariant 2)", ErrInvalidConversation, elideConv(id))
	}
	if err := ids.ValidateBusID(busID); err != nil {
		return fmt.Errorf("%w: conversation_id %q has an invalid bus half: %v", ErrInvalidConversation, elideConv(id), err)
	}
	if err := validateUUIDv4(uuid); err != nil {
		return fmt.Errorf("%w: conversation_id %q has an invalid uuid half: %v", ErrInvalidConversation, elideConv(id), err)
	}
	return nil
}

// validateUUIDv4 accepts exactly a canonical lowercase RFC 4122 version-4 UUID.
//
// One canonical spelling is required, not merely a parseable one — the same
// single-spelling discipline ids.ValidateAgentName and validateSeqSpelling take,
// for the same reason: the id is a routing subject, and admitting an alternate
// spelling (uppercase hex, say) would let two strings name one conversation. The
// server always mints the lowercase form (newUUIDv4), so a record read back off
// OUR disk always matches; anything else is a tampered or foreign id and is
// refused.
func validateUUIDv4(s string) error {
	if len(s) != 36 {
		return fmt.Errorf("uuid is %d bytes, want 36", len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return fmt.Errorf("uuid byte %d is %q, want '-'", i, string(c))
			}
			continue
		}
		if !isLowerHex(c) {
			return fmt.Errorf("uuid byte %d is not lowercase hex", i)
		}
	}
	// Version nibble (octet 6, hex position 14) must be '4'.
	if s[14] != '4' {
		return fmt.Errorf("uuid version nibble is %q, want '4' (UUIDv4)", string(s[14]))
	}
	// Variant nibble (octet 8, hex position 19) must be one of 8,9,a,b (10xxxxxx).
	switch s[19] {
	case '8', '9', 'a', 'b':
	default:
		return fmt.Errorf("uuid variant nibble is %q, want one of 8,9,a,b (RFC 4122)", string(s[19]))
	}
	return nil
}

func isLowerHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// validateConversationName enforces the CONV-NAME-INV6 bound. Empty is allowed;
// when present the name must be <= MaxConversationNameBytes bytes, valid UTF-8,
// single-line and printable — no C0/C1 control codepoint (U+0000–U+001F,
// U+007F–U+009F), no U+2028, no U+2029, no invalid UTF-8. On violation it
// REFUSES; truncation would silently alter operator-supplied identity.
//
// It is called on BOTH the construction path (Encode) and the disk-decode path
// (DecodeConversationRecord). That dual enforcement is the crux of the ruling: a
// name is metadata only because this bound holds everywhere it can enter the
// system, including from a tampered log.
func validateConversationName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > MaxConversationNameBytes {
		return fmt.Errorf("%w: name is %d bytes, over the %d-byte limit; it is refused, not truncated (a truncated label silently alters operator-supplied identity)", ErrInvalidConversation, len(name), MaxConversationNameBytes)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("%w: name is not valid UTF-8; a name written verbatim into the append-only log must be a safe structural label", ErrInvalidConversation)
	}
	for _, r := range name {
		if isDisallowedNameRune(r) {
			// The offending codepoint is reported as U+XXXX, never rendered raw: a
			// control character echoed into a log line is the exact injection this
			// bound closes.
			return fmt.Errorf("%w: name carries a disallowed codepoint U+%04X; the name must be single-line printable UTF-8 (no C0/C1 control, no U+2028/U+2029) to prevent log injection and mis-rendering at rest", ErrInvalidConversation, r)
		}
	}
	return nil
}

// isDisallowedNameRune reports a codepoint the name may not contain: any C0
// control (U+0000–U+001F, which includes tab, newline and carriage return), DEL
// and the C1 controls (U+007F–U+009F), and the Unicode line/paragraph separators
// U+2028 and U+2029. Permitting any of these would allow log injection (an
// embedded newline forging a second audit line), audit-line spoofing, or
// terminal escape sequences that mis-render when an operator reads the log.
func isDisallowedNameRune(r rune) bool {
	switch {
	case r <= 0x1F:
		return true
	case r >= 0x7F && r <= 0x9F:
		return true
	case r == 0x2028 || r == 0x2029:
		return true
	default:
		return false
	}
}

// validate is the single definition of a well-formed conversation record, called
// by Encode (construction) and DecodeConversationRecord (disk). Bounding every
// field in one place is what makes the dual enforcement CONV-RECORD requires
// impossible to get inconsistent between the two paths.
func (r ConversationRecord) validate() error {
	if err := validateConversationID(r.ID); err != nil {
		return err
	}
	if err := validateConversationAgentID("creator", r.Creator); err != nil {
		return err
	}
	if err := validateConversationName(r.Name); err != nil {
		return err
	}
	if len(r.Recipients) == 0 {
		return fmt.Errorf("%w: a conversation has at least one recipient", ErrInvalidConversation)
	}
	if len(r.Recipients) > MaxConversationRecipients {
		return fmt.Errorf("%w: %d recipients, the limit is %d", ErrInvalidConversation, len(r.Recipients), MaxConversationRecipients)
	}
	seen := make(map[string]struct{}, len(r.Recipients))
	for _, rcpt := range r.Recipients {
		if err := validateConversationAgentID("recipient", rcpt); err != nil {
			return err
		}
		if _, dup := seen[rcpt]; dup {
			return fmt.Errorf("%w: recipient %q appears more than once; a membership list names each agent at most once", ErrInvalidConversation, elideConv(rcpt))
		}
		seen[rcpt] = struct{}{}
	}
	if r.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at is required", ErrInvalidConversation)
	}
	return nil
}

// validateConversationAgentID bounds and parses a fully-qualified agent id
// (invariant 2). The length is checked before the parse for the reason
// validateConversationID gives.
func validateConversationAgentID(field, v string) error {
	if v == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidConversation, field)
	}
	if len(v) > ids.MaxAgentIDLen {
		return fmt.Errorf("%w: %s is %d bytes, over the %d-byte limit", ErrInvalidConversation, field, len(v), ids.MaxAgentIDLen)
	}
	if _, _, _, err := ids.ParseAgentID(v); err != nil {
		return fmt.Errorf("%w: %s %q is not a fully-qualified <bus-id>.<agent-id> (invariant 2): %v", ErrInvalidConversation, field, elideConv(v), err)
	}
	return nil
}

// Encode produces the canonical durable bytes. It validates FIRST, so this
// package can never write a record it would refuse to replay.
func (r ConversationRecord) Encode() (json.RawMessage, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	j := conversationRecordJSON{
		Version:    conversationRecordVersion,
		ID:         r.ID,
		Creator:    r.Creator,
		Name:       r.Name,
		Recipients: r.Recipients,
		CreatedAt:  r.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(j); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConversation, err)
	}
	// Encoder.Encode terminates with a newline; the WAL carrier is
	// length-delimited and needs no terminator.
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// DecodeConversationRecord parses a conversation record read back off disk.
//
// It is STRICT in exactly the way ack.DecodeRecord and wal.decodePayload are:
// unknown fields refused, trailing data refused, every field RE-VALIDATED. This
// is the disk-decode half of CONV-RECORD's dual enforcement: a record read back
// from a tampered log with an over-long or control-bearing name, a malformed id,
// or an unbounded recipient list is REFUSED here, not trusted. A lenient decoder
// is how a file that no longer says what history was accepted gets served as if
// it did.
func DecodeConversationRecord(b []byte) (ConversationRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var j conversationRecordJSON
	if err := dec.Decode(&j); err != nil {
		// ELIDED, not %v: encoding/json quotes the offending field name back
		// verbatim, so a damaged record with a huge unknown field would otherwise
		// produce a huge error string.
		return ConversationRecord{}, fmt.Errorf("%w: %s", ErrInvalidConversation, elideConv(err.Error()))
	}
	if dec.More() {
		return ConversationRecord{}, fmt.Errorf("%w: trailing data after the record", ErrInvalidConversation)
	}
	if j.Version != conversationRecordVersion {
		return ConversationRecord{}, fmt.Errorf("%w: record_version %d is not %d; a record from another schema version is REFUSED rather than read with this version's field meanings", ErrInvalidConversation, j.Version, conversationRecordVersion)
	}
	createdAt, err := parseConversationTime("created_at", j.CreatedAt)
	if err != nil {
		return ConversationRecord{}, err
	}
	r := ConversationRecord{
		ID:         j.ID,
		Creator:    j.Creator,
		Name:       j.Name,
		Recipients: j.Recipients,
		CreatedAt:  createdAt,
	}
	if err := r.validate(); err != nil {
		return ConversationRecord{}, err
	}
	return r, nil
}

func parseConversationTime(field, v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		// Both the value and the wrapped error are elided: time.ParseError embeds
		// the full value again, so %v would undo the elision applied to v.
		return time.Time{}, fmt.Errorf("%w: %s (%q) is not RFC3339Nano: %s", ErrInvalidConversation, field, elideConv(v), elideConv(err.Error()))
	}
	return t.UTC(), nil
}

// canonicalConversation returns the record as it will be read back off disk,
// together with those bytes. Every live write folds in THIS value rather than the
// one it built, so a live apply and a replayed apply are byte-for-byte the same
// record — the same anti-drift discipline ack.canonical uses.
func canonicalConversation(r ConversationRecord) (ConversationRecord, json.RawMessage, error) {
	body, err := r.Encode()
	if err != nil {
		return ConversationRecord{}, nil, err
	}
	back, err := DecodeConversationRecord(body)
	if err != nil {
		return ConversationRecord{}, nil, err
	}
	return back, body, nil
}

// maxElidedConvChars bounds how much untrusted, file-derived text may appear in
// an error string — the discipline wal's CorruptError and ack's elide both apply,
// because an error message is a place a hostile record gets to choose the size of.
const maxElidedConvChars = 64

func elideConv(s string) string {
	if len(s) <= maxElidedConvChars {
		return s
	}
	return s[:maxElidedConvChars] + "…(elided)"
}
