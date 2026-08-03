package idem

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Record is one durably-remembered applied operation: the scope tuple, the
// payload fingerprint, the MINTED RESULT, and the commit time.
//
// The result is stored, not merely the key, because a key with no stored result
// cannot satisfy IDEM-12's "return the ORIGINAL result verbatim" — a server
// that remembers only that it has seen a key can refuse a retry or ignore it,
// but it cannot answer it, and answering it is the entire point of invariant
// 10's legitimate-retry carve-out.
//
// CommittedAt is the record's own clock reading and is the ONLY input to
// retention (see Store): the window is a pure predicate over this field, so
// memory and disk can never disagree about which keys are live.
//
// A Record is written to disk inside the SAME two-phase transaction as the
// effect it records — it travels as wal.Entry.Idem. See that field's comment
// for why "same transaction" rather than "written afterwards" is load-bearing.
type Record struct {
	// Agent is the fully-qualified <bus-id>.<agent-id> that performed the
	// operation. It is "" if and only if EnrolBusWide is set.
	Agent string

	// EnrolBusWide marks the one operation with no authenticated caller to
	// scope by — enrolment (doc.go point 4). Its scope is bus-wide.
	EnrolBusWide bool

	// Op is the mutating operation this key was applied to.
	Op Operation

	// Key is the client-supplied idempotency key, validated by ValidateKey.
	Key string

	// Fingerprint is the payload fingerprint (ComputeFingerprint). Comparing it
	// is what separates a legitimate RETRY from a key REUSED for different
	// content.
	Fingerprint Fingerprint

	// Result is the minted result, verbatim, as the route returned it. It is
	// opaque to this package — each operation defines its own shape — and is
	// capped at MaxResultBytes.
	Result json.RawMessage

	// Seq is the server-minted sequence of the effect, or 0 when the operation
	// mints none (enrol and leave mint no message sequence).
	Seq uint64

	// CommittedAt is when the effect committed. It MUST be the same clock
	// reading the effect itself carries: two readings would let the effect and
	// its applied-key record disagree about when the operation happened, and
	// retention is computed from this one.
	CommittedAt time.Time
}

// recordJSON is the wire shape. Compact, no HTML escaping, RFC3339Nano UTC
// timestamp. agent, enrol_bus_wide, result and seq are omitted when empty or
// zero, so the common per-agent record with no sequence stays small — the
// memory bound in retention.go is computed from the worst case, not this one.
type recordJSON struct {
	Agent        string          `json:"agent,omitempty"`
	EnrolBusWide bool            `json:"enrol_bus_wide,omitempty"`
	Op           Operation       `json:"op"`
	Key          string          `json:"key"`
	Fingerprint  string          `json:"fp"`
	Result       json.RawMessage `json:"result,omitempty"`
	Seq          uint64          `json:"seq,omitempty"`
	CommittedAt  string          `json:"committed_at"`
}

// Scope rebuilds this record's Scope through NewAgentScope / NewEnrolScope —
// never by assembling the struct by hand. Going through the constructors is
// what keeps a record read off disk subject to exactly the validation a live
// request is subject to; a hand-built Scope would let a corrupt record mint a
// scope no live call could ever produce, and it would then match — or fail to
// match — a legitimate request for reasons nothing could explain.
func (r Record) Scope() (Scope, error) {
	if r.EnrolBusWide {
		return NewEnrolScope(r.Key)
	}
	return NewAgentScope(r.Agent, r.Op, r.Key)
}

// validate checks a Record is self-consistent. It runs on the way out (before
// the durable write) and again on the way in (after decoding), which is the
// only way both "cannot be stored" and "cannot be trusted" are caught.
func (r Record) validate() error {
	if !r.Op.valid() {
		return fmt.Errorf("%w: %q is not one of the fixed mutating operations", ErrInvalidRecord, r.Op)
	}
	if err := ValidateKey(r.Key); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if r.EnrolBusWide {
		if r.Op != OpEnrol {
			return fmt.Errorf("%w: the bus-wide scope exists only for %q, not %q", ErrInvalidRecord, OpEnrol, r.Op)
		}
		if r.Agent != "" {
			// Not echoed: an agent id on a bus-wide record is either corruption
			// or a caller confusing the two scopes, and quoting it back invites
			// exactly the cross-agent leak Scope exists to prevent.
			return fmt.Errorf("%w: a bus-wide enrol record must carry no agent id, but it carries one (%d bytes)", ErrInvalidRecord, len(r.Agent))
		}
	} else if r.Agent == "" {
		return fmt.Errorf("%w: %v", ErrInvalidRecord, ErrInvalidAgent)
	}
	if len(r.Agent) > MaxAgentLen {
		// ENFORCING the size derivation rather than assuming it: MaxRecordBytes
		// counts this field at MaxAgentLen, and a bound nothing checks is a
		// description, not a bound. Not echoed — it is oversized and untrusted,
		// and an attacker choosing the input must not choose a multiple of it
		// back out of a log line (the same discipline ValidateKey uses).
		return fmt.Errorf("%w: agent id is %d bytes, but a record's agent id is at most %d; it is not echoed here because it is oversized", ErrInvalidRecord, len(r.Agent), MaxAgentLen)
	}
	if len(r.Result) > MaxResultBytes {
		return fmt.Errorf("%w: %d bytes, but a stored result is at most %d; the result is not echoed here because it is oversized", ErrResultTooLarge, len(r.Result), MaxResultBytes)
	}
	if r.CommittedAt.IsZero() {
		return fmt.Errorf("%w: committed_at is the zero time, but retention is computed from it, so a record without one can never expire", ErrInvalidRecord)
	}
	// The scope must be CONSTRUCTIBLE, not merely plausible. A record whose
	// scope no constructor would build could never be looked up by a live
	// request — it would be a key remembered and unreachable, occupying the
	// bound while suppressing nothing. This is what catches, for instance, an
	// OpEnrol record that is not marked bus-wide.
	if _, err := r.Scope(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	return nil
}

// Encode renders the record as the opaque JSON that rides in wal.Entry.Idem.
//
// IT VALIDATES BEFORE IT RETURNS. Encode runs BEFORE the durable write, so a
// record that cannot be stored fails the operation with nothing written —
// rather than being discovered at replay time, when the effect is already
// durable and the only remaining options are all bad.
func (r Record) Encode() (json.RawMessage, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	result := r.Result
	if len(result) > 0 {
		// Compacted so a live Remember and a replayed Remember hold identical
		// bytes, the same discipline wal.canonicalBody applies to a body.
		var buf bytes.Buffer
		if err := json.Compact(&buf, result); err != nil {
			return nil, fmt.Errorf("%w: result is not valid JSON: %v", ErrInvalidRecord, err)
		}
		if buf.String() == "null" {
			result = nil
		} else {
			result = json.RawMessage(buf.Bytes())
		}
		if len(result) > MaxResultBytes {
			return nil, fmt.Errorf("%w: %d bytes, but a stored result is at most %d; the result is not echoed here because it is oversized", ErrResultTooLarge, len(result), MaxResultBytes)
		}
	} else {
		result = nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(recordJSON{
		Agent:        r.Agent,
		EnrolBusWide: r.EnrolBusWide,
		Op:           r.Op,
		Key:          r.Key,
		Fingerprint:  hex.EncodeToString(r.Fingerprint[:]),
		Result:       result,
		Seq:          r.Seq,
		CommittedAt:  r.CommittedAt.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	// Encoder.Encode terminates with a newline; the carrier is length-delimited
	// and needs no terminator.
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// DecodeRecord parses an applied-key record read back off disk.
//
// It is STRICT in exactly the way wal.decodePayload is: unknown fields are
// refused, trailing data is refused, and every field is re-validated. A record
// read off disk is UNTRUSTED INPUT (invariant 1) even though this server wrote
// it — because "this server wrote it" is precisely the claim corruption
// disproves, and a lenient decoder here would reinstate a key with a mangled
// scope, a mangled fingerprint, or a commit time that can never expire.
func DecodeRecord(b []byte) (Record, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var j recordJSON
	if err := dec.Decode(&j); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if dec.More() {
		return Record{}, fmt.Errorf("%w: trailing data after the record", ErrInvalidRecord)
	}
	fp, err := hex.DecodeString(j.Fingerprint)
	if err != nil {
		return Record{}, fmt.Errorf("%w: fingerprint is not hex: %v", ErrInvalidRecord, err)
	}
	if len(fp) != FingerprintSize {
		return Record{}, fmt.Errorf("%w: fingerprint is %d bytes, want %d", ErrInvalidRecord, len(fp), FingerprintSize)
	}
	committedAt, err := time.Parse(time.RFC3339Nano, j.CommittedAt)
	if err != nil {
		return Record{}, fmt.Errorf("%w: committed_at is not RFC3339Nano: %v", ErrInvalidRecord, err)
	}
	r := Record{
		Agent:        j.Agent,
		EnrolBusWide: j.EnrolBusWide,
		Op:           j.Op,
		Key:          j.Key,
		Result:       j.Result,
		Seq:          j.Seq,
		CommittedAt:  committedAt.UTC(),
	}
	copy(r.Fingerprint[:], fp)
	if err := r.validate(); err != nil {
		return Record{}, err
	}
	return r, nil
}
