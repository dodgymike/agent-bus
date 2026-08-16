package ack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
)

// RecordKind is the wal.Entry.Kind discriminator for a delivery lifecycle
// record.
//
// Entry.Kind is a FREE-FORM APPLICATION DISCRIMINATOR that sits INSIDE the
// prepare payload, above the framing layer — it is not a numbered frame type.
// So NO record-type reservation is needed for it and internal/wal/format.go's
// Type enum is untouched. This is written down, exactly as auth.RecordKind,
// invite.RecordKind and relay.OutboxRecordKind write it down, so the next reader
// does not go and reserve a number that nothing requires
// (CONTRACTS-ONDISK.md:825-827).
//
// A NUMBER WOULD HAVE TO BE RESERVED, NOT PICKED, if this design ever did need
// one: `record-type` and `ondisk-format-version` are Spec Server reservation
// namespaces precisely because two parallel agents once both "chose" 024. This
// design needs neither.
const RecordKind = "ack"

// recordVersion is the schema version of the durable record.
//
// It is written and it is CHECKED: a record from a future version is refused
// rather than read with today's field meanings, which is the same
// reject-never-default posture ParseState takes. It is NOT the relay wire
// version (`relay-wire-version` = 1, reserved and unspent — ACK-3 spends it) and
// the two must not be conflated: this number describes bytes on OUR disk, that
// one describes bytes on a peer's wire.
const recordVersion = 1

var (
	// ErrInvalidRecord is a record this bus refuses to write or to read back.
	ErrInvalidRecord = errors.New("ack: invalid delivery lifecycle record")

	// ErrNotDurable is returned by every mutating method before Attach. It is a
	// REFUSAL, not a degraded in-memory mode: a lifecycle table with no log
	// would report outcomes that no restart could reproduce, and invariant 4
	// has no best-effort setting.
	ErrNotDurable = errors.New("ack: the delivery lifecycle table has no durable log attached")

	// ErrNoRecord is ACK-CONTRACT.md §8.2 note 1: no record exists for this
	// (correlation key, recipient), so nothing binds the caller's claim. The
	// caller refuses with §13.3's uniform answer, logs it, and DOES NOT
	// DISCONNECT (§12).
	ErrNoRecord = errors.New("ack: no delivery lifecycle record for that correlation key and recipient")

	// ErrTerminal is §8.2 note 4: a DIFFERENT terminal outcome for a pair that
	// already has one. Invariant 10's second case — reject and log, DO NOT
	// DISCONNECT. The FIRST terminal stands.
	ErrTerminal = errors.New("ack: the delivery lifecycle record is already terminal and terminal is absorbing")

	// ErrCapacity is the hard entry cap, fail-closed, evicting nothing.
	ErrCapacity = errors.New("ack: the delivery lifecycle table is at its hard entry cap")

	// ErrAgentQuota is the per-sender fair share under pressure, fail-closed,
	// evicting nothing.
	ErrAgentQuota = errors.New("ack: this sender holds its fair share of the delivery lifecycle table")

	// ErrConcurrentTransition means another transition for the SAME (correlation
	// key, recipient) is between its decision and its fsync.
	//
	// It is a REFUSAL rather than a wait, and the caller must treat it as one:
	// the outcome it offered was NOT recorded. Waiting would hold a request
	// against a disk write, and answering "accepted" would mean two callers
	// offering CONTRADICTING terminals were both told theirs stood.
	//
	// It carries NO information about the row, so it does not open the oracle
	// §13.3 closes: a caller cannot tell from it whether the key exists.
	ErrConcurrentTransition = errors.New("ack: another transition for this correlation key and recipient is being made durable")
)

// Record is ONE (correlation key, recipient) pair's durable lifecycle state
// (ACK-CONTRACT.md §7.2).
//
// # IT IS KEYED ON (CORRELATION KEY, RECIPIENT) FROM DAY ONE
//
// Never on the correlation key alone. A directed message with N recipients
// produces N rows. This costs nothing today (store.MaxRecipients is 64) and it
// is the single decision that lets broadcast aggregation be specified later —
// when SIGN-3 defines a canonical audience — with NO on-disk change, no new
// record type and no migration (§3.2, §5.5).
//
// # WHAT IT DELIBERATELY DOES NOT CARRY
//
// The body; ANY hash of the body (the audit trail already holds the content
// hash, and duplicating it here creates two fields that must agree); Seq; Pos.
// It carries NO ordering axis at all, which is precisely why the correlation key
// is safe to key on: OriginMessageID takes part in no ordering, no cursor and no
// retention decision (internal/store/message.go:167-194). Seq vs Pos vs
// OriginMessageID has already produced three defects in this repository; this
// record adds no fourth axis.
//
// # THERE IS NO VARIABLE-LENGTH FREE-TEXT FIELD, BY CONSTRUCTION
//
// Every field is a server-minted id, a closed enum or a timestamp. That is what
// makes the worst-case footprint FIXED and therefore what makes MaxEntries a
// bound rather than a hope (see retention.go).
type Record struct {
	// CorrelationKey is the ORIGIN bus's server-minted message id, reached
	// through store.Message.OriginID(): "<origin-bus>-<seq>" (ids.MessageID).
	//
	// IT IS NOT A FOURTH IDENTIFIER — it is the existing third one (§3). It is
	// server-minted BY THE ORIGIN BUS, never sender-supplied, bus-namespaced
	// (invariants 1 and 2) and therefore globally unambiguous with no registry.
	// A key supplied by a client on a future status API is INPUT TO BE
	// VALIDATED, never an identity to be trusted.
	CorrelationKey string

	// Recipient is the fully-qualified "<bus-id>.<agent-id>" this row is about
	// (invariant 2). Never shortened: the namespacing is what makes cross-bus
	// correlation unambiguous.
	Recipient string

	// Sender is the authenticated principal that sent the message, fully
	// qualified. It is what authorises the future status read: only the ORIGINAL
	// SENDER may read a row, and every other case — key never existed, key
	// swept, key belongs to someone else — gets the SAME answer, `unknown`
	// (§13.3). A 403 would confirm the message exists, which is the oracle
	// ACK-4 is required to close.
	Sender string

	// State is the lifecycle state. The two fields below are valid if and only
	// if State names their event; validate enforces that in BOTH directions —
	// present-when-required and absent-when-forbidden — which is the rule
	// OutboxRecord.Reason already lives by (internal/relay/outbox.go:435-441).
	State State

	// Class is the closed-set reason. Set IFF State is a NEGATIVE terminal,
	// forbidden otherwise, and the HALF of the set must match the state:
	// `refused` (the recipient spoke) takes a recipient class, `undeliverable`
	// (the bus gave up) takes a bus class.
	Class Class

	// AttestedBy labels what authenticated the outcome. Set IFF State is
	// terminal. There is no value meaning "verified" (§6.3).
	AttestedBy Attestation

	// AcceptedAt is when this bus committed and fsynced the message, by THIS
	// bus's clock — as store.Message.SentAt is. It is the retention anchor for a
	// NON-terminal row, and it is PRESERVED ACROSS EVERY TRANSITION: a later
	// state records when the message was accepted, not when the transition
	// happened, so the 24h window can never be pushed out by activity.
	AcceptedAt time.Time

	// SettledAt is when the row reached a terminal state. Set IFF terminal, and
	// it is the retention anchor for a terminal row — so a terminal record
	// without one could never be swept, which is why its absence is an error
	// rather than a default.
	SettledAt time.Time
}

// recordJSON is the durable shape: compact, no HTML escaping, omitempty on
// everything optional, the enums fixed STRINGS and the times RFC3339Nano in UTC
// — so an operator can read a row straight out of the WAL with `head -c` and a
// JSON pretty-printer.
type recordJSON struct {
	Version        int    `json:"record_version"`
	CorrelationKey string `json:"correlation_key"`
	Recipient      string `json:"recipient"`
	Sender         string `json:"sender"`
	State          string `json:"state"`
	Class          string `json:"class,omitempty"`
	AttestedBy     string `json:"attested_by,omitempty"`
	AcceptedAt     string `json:"accepted_at"`
	SettledAt      string `json:"settled_at,omitempty"`
}

// key is the composite identity of a row.
//
// It is a STRUCT rather than a concatenated string on purpose: a separator
// between two operator-influenced ids is one more thing that has to be proven
// unambiguous, and relay's job id had to argue that case in a paragraph
// (outbox.go:95-101). A struct key has nothing to argue.
type key struct {
	correlationKey string
	recipient      string
}

func (r Record) key() key {
	return key{correlationKey: r.CorrelationKey, recipient: r.Recipient}
}

// OriginBus is the bus that accepted the message from its own agent: the bus
// half of the correlation key. DERIVED, never stored — two fields that must
// agree are two fields that can disagree.
func (r Record) OriginBus() string {
	busID, _, err := ids.ParseMessageID(r.CorrelationKey)
	if err != nil {
		return ""
	}
	return busID
}

// Expired reports that this row is past its retention window (§11).
//
// The anchor differs by state ON PURPOSE: a non-terminal row is measured from
// acceptance, because that is when the clock on "could this still change"
// started; a terminal row is measured from settlement, because a terminal
// outcome is worth retaining for the full window AFTER it is known.
func (r Record) Expired(now time.Time, retention time.Duration) bool {
	anchor := r.AcceptedAt
	if r.State.Terminal() {
		anchor = r.SettledAt
	}
	if anchor.IsZero() {
		// Unreachable for a record that came out of Encode or DecodeRecord —
		// validate proves both anchors are present when required. Treated as
		// expired rather than immortal: a row that can never be swept is a leak
		// with a bound written on it.
		return true
	}
	return !now.Before(anchor.Add(retention))
}

func (r Record) validate() error {
	if err := validateMessageID("correlation_key", r.CorrelationKey); err != nil {
		return err
	}
	if err := validateAgentID("recipient", r.Recipient); err != nil {
		return err
	}
	if err := validateAgentID("sender", r.Sender); err != nil {
		return err
	}
	if _, ok := stateNames[r.State]; !ok {
		return fmt.Errorf("%w: state %s is outside the closed set", ErrInvalidRecord, r.State)
	}
	// THE CLASS RULE, ENFORCED IN BOTH DIRECTIONS. Required where it is the only
	// thing that stops a discard being silent (invariant 6), and FORBIDDEN
	// everywhere else so a positive outcome cannot grow an explanation channel
	// (§5.4).
	switch {
	case r.State.Negative():
		if r.Class == "" {
			return fmt.Errorf("%w: state %s is a negative terminal and carries no class; a record that cannot say why is a silent discard with a timestamp (invariant 6)", ErrInvalidRecord, r.State)
		}
		if r.State == StateRefused && !r.Class.RecipientEmitted() {
			return fmt.Errorf("%w: state refused carries class %q, which is not one of the three RECIPIENT-emitted classes; `refused` means the recipient application spoke, and recording a routing failure under it would attribute the refusal to the wrong party", ErrInvalidRecord, elide(string(r.Class)))
		}
		if r.State == StateUndeliverable && !r.Class.BusEmitted() {
			return fmt.Errorf("%w: state undeliverable carries class %q, which is not one of the nine BUS-emitted classes; `undeliverable` means this bus gave up, and recording it under a recipient class would claim the application refused a message it may never have seen", ErrInvalidRecord, elide(string(r.Class)))
		}
	case r.Class != "":
		return fmt.Errorf("%w: state %s carries class %q; a class is set IFF the state is a negative terminal, and a positive or in-progress outcome has nothing to explain (§5.4)", ErrInvalidRecord, r.State, elide(string(r.Class)))
	}
	// THE ATTESTATION RULE, also both directions. A non-terminal row has nobody
	// attesting anything yet; a terminal one was produced by SOME party and the
	// record must say which layer that was, because the status API must LABEL
	// attestation rather than imply it (§6.3).
	switch {
	case r.State.Terminal():
		if !r.AttestedBy.Valid() {
			return fmt.Errorf("%w: state %s carries attestation %q, which is outside the closed set (peer_bus, recipient_signature_unverified); there is deliberately no value meaning \"verified\" because nothing can produce one", ErrInvalidRecord, r.State, elide(string(r.AttestedBy)))
		}
	case r.AttestedBy != "":
		return fmt.Errorf("%w: state %s carries attestation %q; attestation is set IFF the state is terminal", ErrInvalidRecord, r.State, elide(string(r.AttestedBy)))
	}
	if r.AcceptedAt.IsZero() {
		return fmt.Errorf("%w: accepted_at is required on every record; it is the retention anchor for a non-terminal row", ErrInvalidRecord)
	}
	if r.State.Terminal() != !r.SettledAt.IsZero() {
		if r.State.Terminal() {
			return fmt.Errorf("%w: state %s is terminal and carries no settled_at; it is the input to the retention sweep, so a terminal record without one could never be swept", ErrInvalidRecord, r.State)
		}
		return fmt.Errorf("%w: state %s is not terminal and carries settled_at; a row that has not settled has no settlement time", ErrInvalidRecord, r.State)
	}
	// DELIBERATELY NOT CHECKED: that SettledAt is at or after AcceptedAt. The
	// two are separate readings of a wall clock that an operator may step
	// backwards, and a record refused for that reason is a DURABLE, already
	// acknowledged outcome that memory would then disagree with. Ordering is not
	// load-bearing here — nothing sorts on either field, and both are only ever
	// compared against `now` by the sweep — so enforcing it would buy a fail
	// mode and no property.
	return nil
}

// validateMessageID bounds and parses a correlation key.
//
// The LENGTH is checked BEFORE the parse so a hostile or damaged record cannot
// choose how much text a parse error quotes back — the bound-before-quote
// discipline the whole durable layer uses.
func validateMessageID(field, v string) error {
	if v == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidRecord, field)
	}
	if len(v) > ids.MaxMessageIDLen {
		return fmt.Errorf("%w: %s is %d bytes, over the %d-byte limit", ErrInvalidRecord, field, len(v), ids.MaxMessageIDLen)
	}
	if _, _, err := ids.ParseMessageID(v); err != nil {
		return fmt.Errorf("%w: %s %q is not a server-minted message id: %s", ErrInvalidRecord, field, elide(v), elide(err.Error()))
	}
	return nil
}

// validateAgentID bounds and parses a fully-qualified agent id (invariant 2).
func validateAgentID(field, v string) error {
	if v == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidRecord, field)
	}
	if len(v) > ids.MaxAgentIDLen {
		return fmt.Errorf("%w: %s is %d bytes, over the %d-byte limit", ErrInvalidRecord, field, len(v), ids.MaxAgentIDLen)
	}
	if _, _, _, err := ids.ParseAgentID(v); err != nil {
		return fmt.Errorf("%w: %s %q is not a fully-qualified <bus-id>.<agent-id> (invariant 2): %s", ErrInvalidRecord, field, elide(v), elide(err.Error()))
	}
	return nil
}

// Encode produces the canonical durable bytes. It validates FIRST, so this
// package can never write a record it would refuse to replay.
func (r Record) Encode() (json.RawMessage, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	j := recordJSON{
		Version:        recordVersion,
		CorrelationKey: r.CorrelationKey,
		Recipient:      r.Recipient,
		Sender:         r.Sender,
		State:          r.State.String(),
		Class:          string(r.Class),
		AttestedBy:     string(r.AttestedBy),
		AcceptedAt:     r.AcceptedAt.UTC().Format(time.RFC3339Nano),
	}
	// The terminal field is written ONLY for the states that own it, so the
	// encoder cannot produce a record its own validate would refuse on the way
	// back in.
	if r.State.Terminal() {
		j.SettledAt = r.SettledAt.UTC().Format(time.RFC3339Nano)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(j); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	// Encoder.Encode terminates with a newline; the WAL carrier is
	// length-delimited and needs no terminator.
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// DecodeRecord parses a lifecycle record read back off disk.
//
// It is STRICT in exactly the way wal.decodePayload, invite.DecodeRecord and
// relay.DecodeOutboxRecord are: unknown fields refused, trailing data refused,
// every field re-validated. A lenient decoder would reinstate a row with a
// mangled state — and the worst of those failures reinstates a TERMINAL row as a
// non-terminal one, which resurrects a settled outcome.
func DecodeRecord(b []byte) (Record, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var j recordJSON
	if err := dec.Decode(&j); err != nil {
		// ELIDED, not %v. encoding/json quotes the offending field name back
		// verbatim and time.ParseError quotes its value TWICE, so a damaged
		// record with a 200 KiB unknown field would otherwise produce a
		// 200-400 KiB error string.
		return Record{}, fmt.Errorf("%w: %s", ErrInvalidRecord, elide(err.Error()))
	}
	if dec.More() {
		return Record{}, fmt.Errorf("%w: trailing data after the record", ErrInvalidRecord)
	}
	if j.Version != recordVersion {
		return Record{}, fmt.Errorf("%w: record_version %d is not %d; a record from another schema version is REFUSED rather than read with this version's field meanings", ErrInvalidRecord, j.Version, recordVersion)
	}
	state, err := ParseState(j.State)
	if err != nil {
		return Record{}, err
	}
	acceptedAt, err := parseRecordTime("accepted_at", j.AcceptedAt)
	if err != nil {
		return Record{}, err
	}
	r := Record{
		CorrelationKey: j.CorrelationKey,
		Recipient:      j.Recipient,
		Sender:         j.Sender,
		State:          state,
		Class:          Class(j.Class),
		AttestedBy:     Attestation(j.AttestedBy),
		AcceptedAt:     acceptedAt,
	}
	if j.SettledAt != "" {
		if r.SettledAt, err = parseRecordTime("settled_at", j.SettledAt); err != nil {
			return Record{}, err
		}
	}
	if err := r.validate(); err != nil {
		return Record{}, err
	}
	return r, nil
}

func parseRecordTime(field, v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		// Both the value AND the wrapped error are elided: time.ParseError
		// embeds the full value a second time, so quoting it with %v would undo
		// the elision applied to v.
		return time.Time{}, fmt.Errorf("%w: %s (%q) is not RFC3339Nano: %s", ErrInvalidRecord, field, elide(v), elide(err.Error()))
	}
	return t.UTC(), nil
}

// maxElidedChars bounds how much untrusted, file-derived text may appear in an
// error string — the discipline wal's CorruptError and relay's elideOutbox both
// apply, for the same reason: an error message is a place a hostile record gets
// to choose the size of.
const maxElidedChars = 64

func elide(s string) string {
	if len(s) <= maxElidedChars {
		return s
	}
	return s[:maxElidedChars] + "…(elided)"
}

// canonical returns the record as it will be read back off disk, together with
// those bytes.
//
// Every mutating path folds in THIS value rather than the one it built, so a
// live apply and a replayed apply are byte-for-byte the same record. Without it
// the two can drift in ways that are invisible until a restart disagrees with a
// running bus — a UTC normalisation, a monotonic clock reading, a nanosecond
// that RFC3339Nano rounds.
func canonical(r Record) (Record, json.RawMessage, error) {
	body, err := r.Encode()
	if err != nil {
		return Record{}, nil, err
	}
	back, err := DecodeRecord(body)
	if err != nil {
		return Record{}, nil, err
	}
	return back, body, nil
}
