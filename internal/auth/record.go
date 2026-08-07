package auth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
)

// RecordKind is the wal.Entry.Kind discriminator for an enrolment. wal does not
// interpret it; it exists so a replay can tell an enrolment record from the
// message records (store.RecordKind) that share the same log.
//
// NO RESERVATION WAS TAKEN FOR IT, AND NONE IS NEEDED. wal.Entry.Kind is a
// free-form application STRING, not a reserved on-disk record-type NUMBER — the
// numbers (wal.Type) are owned by internal/wal and are reserved through the
// Spec Server; this is not one of them. "agent" is moreover the exact name
// internal/wal/log.go's own Entry.Kind doc already gives this discriminator
// ("the application discriminator: \"message\", \"agent\", ..."), so it is the
// name the format was written expecting, not a name invented here.
const RecordKind = "agent"

// RecordVersion is the schema version of the JSON payload inside an enrolment
// wal.Entry. It is NOT the on-disk WAL format version (that is reserved and
// owned by internal/wal) and it is NOT the HTTP API version: it versions only
// the field set of recordJSON below.
//
// Bumping it is a last resort. The record is deliberately shaped to carry the
// WHOLE ENROL-SHAPE field set from version 1 — including fields nothing
// populates yet — precisely so the INVITE, MTLS-BIND and SIGN epics can fill
// them in without a bump. Adding an optional field does not move this number;
// changing the meaning of an existing one does.
const RecordVersion = 1

// recordJSON is the ON-DISK SHAPE of an enrolment. THESE FIELD NAMES ARE
// FOREVER: they are written into an append-only log that later builds must
// still read, so they are chosen once, deliberately, and documented here.
//
//	{"v":1,
//	 "agent_id":"<bus-id>.<name>-<n>",
//	 "name":"<name>",
//	 "auth_pub":"<base64 std, 32 bytes>",
//	 "msg_pub":"<base64 std, 32 bytes>",        // omitted while unpopulated
//	 "invite_id":"<string>",                    // omitted while unpopulated
//	 "epoch":"<RFC3339Nano UTC>",
//	 "cert_bindings":[{"fp":"<hex, 32 bytes>",
//	                   "bound_at":"<RFC3339Nano UTC>",
//	                   "retired_at":"<RFC3339Nano UTC>"}],  // omitted while live
//	 "enrolled_at":"<RFC3339Nano UTC>"}
//
// The encodings match the precedents already on disk rather than being picked
// per field: times are RFC3339Nano in UTC exactly as idem.Record writes
// committed_at; the certificate fingerprint is HEX, the same choice idem makes
// for its own "fp"; and the public keys are BASE64 STANDARD ENCODING, which is
// what the enrolment wire format already uses for the same bytes — so an
// operator reading the log sees the same string the client sent.
//
// Every reserved field is OMITEMPTY. A record written today is therefore
// byte-for-byte the record a pre-INVITE, pre-MTLS build would have written, and
// the reserved keys appear on disk only once something actually populates them.
type recordJSON struct {
	V          int               `json:"v"`
	AgentID    string            `json:"agent_id"`
	Name       string            `json:"name"`
	AuthPub    string            `json:"auth_pub"`
	MsgPub     string            `json:"msg_pub,omitempty"`
	InviteID   string            `json:"invite_id,omitempty"`
	Epoch      string            `json:"epoch"`
	CertBinds  []certBindingJSON `json:"cert_bindings,omitempty"`
	EnrolledAt string            `json:"enrolled_at"`
}

// certBindingJSON is the on-disk shape of one CertBinding. retired_at is
// omitted entirely while the binding is LIVE, which is what makes "live" and
// "retired at the zero time" impossible to confuse — retirement is explicit
// (rule 3 of ENROL-SHAPE) and an absent key is the only way to say "not
// retired".
type certBindingJSON struct {
	Fingerprint string `json:"fp"`
	BoundAt     string `json:"bound_at"`
	RetiredAt   string `json:"retired_at,omitempty"`
}

// Encode renders a RosterEntry as the JSON body of a wal.Entry.
//
// IT VALIDATES BEFORE IT RETURNS, for the reason idem.Record.Encode does:
// Encode runs BEFORE the durable write, so a record that cannot be stored fails
// the whole operation with NOTHING written — rather than being discovered as
// broken at replay time, when the enrolment is already durable, the client has
// already been told its agent id, and every remaining option is bad.
//
// It is exported so tests and a future peer-roster relay can produce the same
// bytes this package writes, rather than a second encoder that drifts.
func Encode(e RosterEntry) (json.RawMessage, error) {
	if err := validateRosterEntry(e); err != nil {
		return nil, err
	}

	binds := make([]certBindingJSON, 0, len(e.CertBindings))
	for _, b := range e.CertBindings {
		j := certBindingJSON{
			Fingerprint: hex.EncodeToString(b.Fingerprint[:]),
			BoundAt:     b.BoundAt.UTC().Format(time.RFC3339Nano),
		}
		if b.RetiredAt != nil {
			j.RetiredAt = b.RetiredAt.UTC().Format(time.RFC3339Nano)
		}
		binds = append(binds, j)
	}
	if len(binds) == 0 {
		// nil, not an empty slice: omitempty then drops the key, so a record
		// with no bindings is byte-identical to one written before the field
		// existed.
		binds = nil
	}

	rec := recordJSON{
		V:          RecordVersion,
		AgentID:    e.AgentID,
		Name:       e.Name,
		AuthPub:    base64.StdEncoding.EncodeToString(e.AuthPublicKey),
		Epoch:      e.Epoch.UTC().Format(time.RFC3339Nano),
		CertBinds:  binds,
		EnrolledAt: e.EnrolledAt.UTC().Format(time.RFC3339Nano),
	}
	if len(e.MessagingPublicKey) != 0 {
		rec.MsgPub = base64.StdEncoding.EncodeToString(e.MessagingPublicKey)
	}
	rec.InviteID = e.InviteID

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	// Encoder.Encode terminates with a newline; the carrier is length-delimited
	// and needs no terminator.
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// Decode parses an enrolment record read back off disk.
//
// A record read off disk is UNTRUSTED INPUT (invariant 1) even though this
// server wrote it — "this server wrote it" is exactly the claim corruption
// disproves — so every field is re-validated: the version must be this build's,
// the agent id must parse, the name must match the id byte-identically, the
// auth key must be exactly ed25519.PublicKeySize, the messaging key must be
// absent or exactly that size, and the certificate history must be within
// MaxCertBindings.
//
// # Unknown fields are REFUSED, and the downgrade hazard that buys
//
// The decoder is strict, matching idem.DecodeRecord and wal.decodePayload. The
// cost is the same one wal.Entry.Idem documents: a binary built BEFORE a field
// existed, reading a log written AFTER it, sees EVERY record carrying that
// field as undecodable — and an undecodable enrolment record is DISCARDED
// (WALRoster.Apply), so the agent is silently absent and must re-enrol.
// Downgrade is not a supported operation here (one binary, one container,
// forward-only), and a lenient decoder is how a record whose extra field
// CHANGES the meaning of the ones it does understand — a retired-at, a
// revocation marker, a key-usage flag — gets served as if it did not. Better a
// record this build refuses to interpret than one it interprets wrongly, when
// what it is interpreting is which key authenticates as which identity.
//
// Note the version check catches the ORDINARY form of that downgrade with a
// much better message; DisallowUnknownFields only catches an additive field
// that did NOT move RecordVersion, which is the case this package intends to
// keep open for the INVITE/MTLS/SIGN epics. Those epics must therefore land
// their field and its decoder together, in one build.
func Decode(raw json.RawMessage) (RosterEntry, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var j recordJSON
	if err := dec.Decode(&j); err != nil {
		return RosterEntry{}, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if dec.More() {
		return RosterEntry{}, fmt.Errorf("%w: trailing data after the record", ErrInvalidRecord)
	}
	if j.V != RecordVersion {
		// A record from a FUTURE build is refused rather than guessed at. It is
		// a distinct message from "malformed" because the remedy is different:
		// the operator downgraded, and the fix is to run the newer binary.
		return RosterEntry{}, fmt.Errorf("%w: record schema version %d, this build understands %d", ErrInvalidRecord, j.V, RecordVersion)
	}

	_, name, _, err := ids.ParseAgentID(j.AgentID)
	if err != nil {
		return RosterEntry{}, fmt.Errorf("%w: agent id: %v", ErrInvalidRecord, err)
	}
	if j.Name != name {
		// Byte-identical, never folded: see RosterEntry.Name. A record whose
		// name half disagrees with its id is either corruption or two different
		// spellings of one agent, and both are identity questions.
		return RosterEntry{}, fmt.Errorf("%w: record name and agent id disagree: the id %q carries name %q", ErrInvalidRecord, j.AgentID, name)
	}

	authPub, err := decodeKey(j.AuthPub)
	if err != nil {
		return RosterEntry{}, fmt.Errorf("%w: auth public key: %v", ErrInvalidRecord, err)
	}
	var msgPub ed25519.PublicKey
	if j.MsgPub != "" {
		msgPub, err = decodeKey(j.MsgPub)
		if err != nil {
			return RosterEntry{}, fmt.Errorf("%w: messaging public key: %v", ErrInvalidRecord, err)
		}
	}

	epoch, err := time.Parse(time.RFC3339Nano, j.Epoch)
	if err != nil {
		return RosterEntry{}, fmt.Errorf("%w: epoch is not RFC3339Nano: %v", ErrInvalidRecord, err)
	}
	enrolledAt, err := time.Parse(time.RFC3339Nano, j.EnrolledAt)
	if err != nil {
		return RosterEntry{}, fmt.Errorf("%w: enrolled_at is not RFC3339Nano: %v", ErrInvalidRecord, err)
	}

	if len(j.CertBinds) > MaxCertBindings {
		// Checked BEFORE the loop allocates: this is a length off DISK, and the
		// bound exists precisely because the startup path must not allocate
		// whatever the file claims.
		return RosterEntry{}, fmt.Errorf("%w: %d certificate bindings, the limit is %d", ErrInvalidRecord, len(j.CertBinds), MaxCertBindings)
	}
	var binds []CertBinding
	if len(j.CertBinds) > 0 {
		binds = make([]CertBinding, 0, len(j.CertBinds))
		for i, b := range j.CertBinds {
			fp, err := hex.DecodeString(b.Fingerprint)
			if err != nil {
				return RosterEntry{}, fmt.Errorf("%w: certificate binding %d: fingerprint is not hex: %v", ErrInvalidRecord, i, err)
			}
			if len(fp) != certFingerprintSize {
				return RosterEntry{}, fmt.Errorf("%w: certificate binding %d: fingerprint is %d bytes, want %d", ErrInvalidRecord, i, len(fp), certFingerprintSize)
			}
			boundAt, err := time.Parse(time.RFC3339Nano, b.BoundAt)
			if err != nil {
				return RosterEntry{}, fmt.Errorf("%w: certificate binding %d: bound_at is not RFC3339Nano: %v", ErrInvalidRecord, i, err)
			}
			cb := CertBinding{BoundAt: boundAt.UTC()}
			copy(cb.Fingerprint[:], fp)
			if b.RetiredAt != "" {
				retiredAt, err := time.Parse(time.RFC3339Nano, b.RetiredAt)
				if err != nil {
					return RosterEntry{}, fmt.Errorf("%w: certificate binding %d: retired_at is not RFC3339Nano: %v", ErrInvalidRecord, i, err)
				}
				u := retiredAt.UTC()
				cb.RetiredAt = &u
			}
			binds = append(binds, cb)
		}
	}

	e := RosterEntry{
		AgentID:            j.AgentID,
		Name:               j.Name,
		AuthPublicKey:      authPub,
		MessagingPublicKey: msgPub,
		InviteID:           j.InviteID,
		Epoch:              epoch.UTC(),
		CertBindings:       binds,
		EnrolledAt:         enrolledAt.UTC(),
	}
	// Re-validated through the SAME predicate Encode used, so "cannot be
	// stored" and "cannot be trusted" are the same rule read in both
	// directions.
	if err := validateRosterEntry(e); err != nil {
		return RosterEntry{}, err
	}
	return e, nil
}

// certFingerprintSize is the length of a CertBinding.Fingerprint, i.e. the
// output of sha256.Sum256. Named rather than written as 32 at each use so the
// on-disk check and the struct field cannot drift.
const certFingerprintSize = 32

// decodeKey parses one base64 standard-encoding Ed25519 public key of exactly
// ed25519.PublicKeySize bytes. A wrong-size key is refused HERE, on the way off
// disk, because ed25519.Verify PANICS on one (see ErrInvalidPublicKey) and the
// session path verifies with whatever the roster holds.
func decodeKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("not base64: %v", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%d bytes, want exactly %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// validateRosterEntry checks an entry is storable: it has a parseable,
// server-shaped agent id whose name half matches Name byte for byte, keys of
// the right size, timestamps that are set, and a bounded certificate history.
//
// It runs on the way OUT (before the durable write) and again on the way IN
// (after decoding), which is the only way both "cannot be stored" and "cannot
// be trusted" are caught.
func validateRosterEntry(e RosterEntry) error {
	if e.AgentID == "" {
		return fmt.Errorf("%w: agent id is empty", ErrInvalidRecord)
	}
	_, name, _, err := ids.ParseAgentID(e.AgentID)
	if err != nil {
		return fmt.Errorf("%w: agent id: %v", ErrInvalidRecord, err)
	}
	if e.Name != name {
		return fmt.Errorf("%w: entry name and agent id disagree: the id %q carries name %q", ErrInvalidRecord, e.AgentID, name)
	}
	if err := validateRosterEntryKeys(e); err != nil {
		return err
	}
	if e.Epoch.IsZero() {
		return fmt.Errorf("%w: agent %q has a zero enrolment epoch; the epoch is stored rather than derived, so a record without one cannot be interpreted after a restart", ErrInvalidRecord, e.AgentID)
	}
	if e.EnrolledAt.IsZero() {
		return fmt.Errorf("%w: agent %q has a zero enrolled_at", ErrInvalidRecord, e.AgentID)
	}
	for i, b := range e.CertBindings {
		if b.BoundAt.IsZero() {
			return fmt.Errorf("%w: agent %q certificate binding %d has a zero bound_at", ErrInvalidRecord, e.AgentID, i)
		}
		if b.RetiredAt != nil && b.RetiredAt.IsZero() {
			// An explicitly-set zero retirement time is worse than none: it
			// says "retired" while carrying no instant, and retirement must be
			// explicit AND legible (rule 3 of ENROL-SHAPE).
			return fmt.Errorf("%w: agent %q certificate binding %d is marked retired at the zero time; a live binding carries no retired_at at all", ErrInvalidRecord, e.AgentID, i)
		}
	}
	return nil
}
