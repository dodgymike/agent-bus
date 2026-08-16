package auth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// OperatorRecordKind is the wal.Entry.Kind discriminator for an operator record
// (AUTH-10). wal does not interpret it; it exists so a replay can tell an
// operator record from the enrolment ("agent"), invite, message and ack records
// that share the same log.
//
// NO RESERVATION WAS TAKEN FOR IT, AND NONE IS NEEDED — the same statement
// RecordKind makes at record.go:15-26, for the same reason. wal.Entry.Kind is a
// free-form application STRING, not a reserved on-disk record-type NUMBER: the
// numbers (wal.Type) are owned by internal/wal and are reserved through the Spec
// Server, and this is not one of them.
const OperatorRecordKind = "operator"

// OperatorRecordVersion is the schema version of the JSON payload inside an
// operator wal.Entry. It is NOT the on-disk WAL format version (that is reserved
// and owned by internal/wal) and it is NOT the HTTP API version: it versions
// only the field set of operatorJSON below.
const OperatorRecordVersion = 1

// MaxOperatorLabelLen bounds Operator.Label.
//
// 128 bytes, the SAME bound invite.MaxLabelLen uses for the operator note on an
// invite, because it is the same kind of value written by the same person at the
// same kind of moment — reusing the number rather than picking a second one
// keeps one answer to "how long may an operator note be" in the system.
const MaxOperatorLabelLen = 128

// Operator is one operator/admin principal: a bus-scoped, NON-AGENT identity
// that can authenticate to the running bus and that operator-only capabilities
// are authorised against.
//
// # THE POINT OF THIS TYPE IS THAT IT IS NOT A RosterEntry
//
// If an admin route reused AGENT authentication, an AGENT credential would
// authorise minting the credentials that CREATE AGENTS: any enrolled agent could
// mint itself an unlimited supply of new identities, which collapses invariant 3
// completely. So the principal is distinct in KIND, not merely in permission —
// a separate id namespace (operatorid.go), a separate durable record (this
// file), a separate registry and a separate Go principal type and session table
// (operator.go, operatorsession.go). None of the four is decorative: a flag on
// RosterEntry would have been one boolean away from being granted by whoever can
// write the roster, and one careless handler away from being satisfied by any
// authenticated agent.
//
// The server never holds an operator's PRIVATE key or its certificate's private
// key. Both are generated on the operator's own machine by
// `agent-bus operator keygen`; what is recorded here is the PUBLIC half and a
// DIGEST, which is why this whole record is safe to print in `operator list`.
type Operator struct {
	// OperatorID is the server-minted "op:<bus-id>.<name>-<suffix>" (invariant
	// 1). It is never reused, including after revocation.
	OperatorID string

	// Name is the short name the operator was added under, byte-identical to
	// the name half of OperatorID — kept separately so a reader does not have to
	// re-parse, and re-checked on both encode and decode for exactly the reason
	// RosterEntry.Name is (a record whose halves disagree is an identity
	// question, not a formatting one).
	Name string

	// AuthPublicKey is the PUBLIC half of the operator's Ed25519 session-signing
	// keypair, exactly ed25519.PublicKeySize bytes. It is what
	// OperatorService.CompleteSession verifies the challenge signature against.
	//
	// REQUIRED. Unlike RosterEntry.MessagingPublicKey there is no
	// "reserved/unpopulated" state here: this record type is new, nothing
	// pre-dates it, so an operator with no key is a record that could never
	// authenticate anybody and is refused rather than stored.
	AuthPublicKey ed25519.PublicKey

	// CertFingerprint is sha256 over the DER of the operator's client
	// certificate — the same construction buscert.FingerprintOf and
	// client.ClientCertificate.Fingerprint produce, so "the fingerprint of a
	// certificate" keeps exactly one spelling in this system.
	//
	// # A CERTIFICATE IS MANDATORY FOR AN OPERATOR, UNLIKE AN AGENT
	//
	// RosterEntry.CertBindings may legitimately be empty: agents enrolled before
	// MTLS-BIND have none, and the listener REQUESTS rather than REQUIRES a
	// client certificate, so an agent with no binding is an ordinary state.
	// An operator with no binding is not, and the difference is deliberate:
	// invariant 11's cross-check ("a session token presented over a connection
	// whose client certificate belongs to a DIFFERENT principal must be
	// rejected") can only be applied UNNARROWED if there is ALWAYS a pair to
	// cross-check. An operator with no fingerprint would be a principal for whom
	// the cross-check silently degrades to "session token only" — which is the
	// bearer-credential-alone model invariant 11 exists to refuse — and it would
	// do so invisibly, because every positive test would still pass.
	//
	// There is exactly ONE fingerprint per operator rather than a bounded
	// history: an operator is a person with a laptop, not a fleet mid-rollover,
	// and rotation is "add a new operator, revoke the old one", which leaves a
	// legible audit trail instead of a silent rebinding of a live identity.
	CertFingerprint [32]byte

	// Label is the adding operator's own note ("mike, laptop, 2026-08"), at most
	// MaxOperatorLabelLen bytes. It is never shown to anybody but whoever reads
	// `operator list`, and nothing authorises on it.
	Label string

	// CreatedAt is when this operator was added.
	CreatedAt time.Time

	// RevokedAt is when it was revoked; nil means LIVE. Revocation is EXPLICIT
	// and is recorded by APPENDING a new record (invariant 6: the log is
	// append-only in the strict sense, no in-place edits and no deletions), so a
	// revoked operator's whole history stays readable and its id stays
	// permanently spent (invariant 1).
	RevokedAt *time.Time

	// RevokedReason is the operator's attribution for the revocation. It is
	// REQUIRED whenever RevokedAt is set: invariant 6 wants an operator action
	// to be loudly attributable, and "revoked at 03:12" with no reason is a fact
	// nobody can act on six months later.
	RevokedReason string
}

// Revoked reports whether this operator has been revoked. It is the ONE
// question every authorisation path asks, so it is a method rather than a
// `RevokedAt != nil` written out at each call site — a nil check that is
// forgotten in one place is a revoked operator that still authenticates.
func (o Operator) Revoked() bool { return o.RevokedAt != nil }

// operatorJSON is the ON-DISK SHAPE of an operator record. THESE FIELD NAMES
// ARE FOREVER: they are written into an append-only log that later builds must
// still read, so they are chosen once, deliberately, and documented here.
//
//	{"v":1,
//	 "operator_id":"op:<bus-id>.<name>-<suffix>",
//	 "name":"<name>",
//	 "auth_pub":"<base64 std, 32 bytes>",
//	 "cert_fp":"<hex, 32 bytes>",
//	 "label":"<text>",                        // omitted when empty
//	 "created_at":"<RFC3339Nano UTC>",
//	 "revoked_at":"<RFC3339Nano UTC>",        // omitted while LIVE
//	 "revoked_reason":"<text>"}               // omitted while LIVE
//
// The encodings match the precedents already on disk rather than being picked
// per field, exactly as recordJSON does: times are RFC3339Nano in UTC, the
// certificate fingerprint is lowercase HEX (idem's and recordJSON's choice for
// the same value), and the public key is BASE64 STANDARD ENCODING, which is what
// the enrolment wire format and recordJSON already use for the same bytes.
//
// revoked_at is OMITTED ENTIRELY while the operator is live, which is what makes
// "live" and "revoked at the zero time" impossible to confuse — the same rule
// certBindingJSON applies to retired_at, and for the same reason.
type operatorJSON struct {
	V             int    `json:"v"`
	OperatorID    string `json:"operator_id"`
	Name          string `json:"name"`
	AuthPub       string `json:"auth_pub"`
	CertFP        string `json:"cert_fp"`
	Label         string `json:"label,omitempty"`
	CreatedAt     string `json:"created_at"`
	RevokedAt     string `json:"revoked_at,omitempty"`
	RevokedReason string `json:"revoked_reason,omitempty"`
}

// EncodeOperator renders an Operator as the JSON body of a wal.Entry.
//
// IT VALIDATES BEFORE IT RETURNS, for the reason Encode and idem.Record.Encode
// do: Encode runs BEFORE the durable write, so a record that cannot be stored
// fails the whole operation with NOTHING written — rather than being discovered
// as broken at replay time, when the record is already durable and every
// remaining option is bad.
func EncodeOperator(o Operator) (json.RawMessage, error) {
	if err := validateOperator(o); err != nil {
		return nil, err
	}

	rec := operatorJSON{
		V:          OperatorRecordVersion,
		OperatorID: o.OperatorID,
		Name:       o.Name,
		AuthPub:    base64.StdEncoding.EncodeToString(o.AuthPublicKey),
		CertFP:     hex.EncodeToString(o.CertFingerprint[:]),
		Label:      o.Label,
		CreatedAt:  o.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if o.RevokedAt != nil {
		rec.RevokedAt = o.RevokedAt.UTC().Format(time.RFC3339Nano)
		rec.RevokedReason = o.RevokedReason
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidOperatorRecord, err)
	}
	// Encoder.Encode terminates with a newline; the carrier is length-delimited
	// and needs no terminator.
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// DecodeOperator parses an operator record read back off disk.
//
// A record read off disk is UNTRUSTED INPUT (invariant 1) even though this
// server wrote it — "this server wrote it" is exactly the claim corruption
// disproves — so every field is re-validated through the SAME predicate
// EncodeOperator used, and unknown fields are REFUSED (Decode's rule, and its
// reasoning about downgrade applies here verbatim: better a record this build
// refuses to interpret than one it interprets wrongly, when what it is
// interpreting is which key authenticates as an ADMIN).
func DecodeOperator(raw json.RawMessage) (Operator, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var j operatorJSON
	if err := dec.Decode(&j); err != nil {
		return Operator{}, fmt.Errorf("%w: %v", ErrInvalidOperatorRecord, err)
	}
	if dec.More() {
		return Operator{}, fmt.Errorf("%w: trailing data after the record", ErrInvalidOperatorRecord)
	}
	if j.V != OperatorRecordVersion {
		// A record from a FUTURE build is refused rather than guessed at. It is
		// a distinct message from "malformed" because the remedy is different:
		// the operator downgraded, and the fix is to run the newer binary.
		return Operator{}, fmt.Errorf("%w: record schema version %d, this build understands %d", ErrInvalidOperatorRecord, j.V, OperatorRecordVersion)
	}

	pub, err := decodeKey(j.AuthPub)
	if err != nil {
		return Operator{}, fmt.Errorf("%w: auth public key: %v", ErrInvalidOperatorRecord, err)
	}

	fpBytes, err := hex.DecodeString(j.CertFP)
	if err != nil {
		return Operator{}, fmt.Errorf("%w: certificate fingerprint is not hex: %v", ErrInvalidOperatorRecord, err)
	}
	if len(fpBytes) != certFingerprintSize {
		return Operator{}, fmt.Errorf("%w: certificate fingerprint is %d bytes, want %d", ErrInvalidOperatorRecord, len(fpBytes), certFingerprintSize)
	}

	createdAt, err := time.Parse(time.RFC3339Nano, j.CreatedAt)
	if err != nil {
		return Operator{}, fmt.Errorf("%w: created_at is not RFC3339Nano: %v", ErrInvalidOperatorRecord, err)
	}

	o := Operator{
		OperatorID:    j.OperatorID,
		Name:          j.Name,
		AuthPublicKey: pub,
		Label:         j.Label,
		CreatedAt:     createdAt.UTC(),
		RevokedReason: j.RevokedReason,
	}
	copy(o.CertFingerprint[:], fpBytes)
	if j.RevokedAt != "" {
		revokedAt, err := time.Parse(time.RFC3339Nano, j.RevokedAt)
		if err != nil {
			return Operator{}, fmt.Errorf("%w: revoked_at is not RFC3339Nano: %v", ErrInvalidOperatorRecord, err)
		}
		u := revokedAt.UTC()
		o.RevokedAt = &u
	}

	if err := validateOperator(o); err != nil {
		return Operator{}, err
	}
	return o, nil
}

// validateOperator checks an operator record is storable AND trustworthy. It
// runs on the way OUT (before the durable write) and again on the way IN (after
// decoding), which is the only way both "cannot be stored" and "cannot be
// trusted" are caught by one rule.
//
// EVERY CHECK HERE FAILS CLOSED. There is no field whose absence is tolerated
// "for now": this record type is new, so there is no pre-existing on-disk
// population whose records a strict rule would lock out — which is precisely the
// consideration that forces RosterEntry to accept an absent messaging key and an
// absent certificate binding.
func validateOperator(o Operator) error {
	if o.OperatorID == "" {
		return fmt.Errorf("%w: operator id is empty", ErrInvalidOperatorRecord)
	}
	_, name, _, err := ParseOperatorID(o.OperatorID)
	if err != nil {
		return fmt.Errorf("%w: operator id: %v", ErrInvalidOperatorRecord, err)
	}
	if o.Name != name {
		// Byte-identical, never folded — RosterEntry.Name's rule. A record whose
		// name half disagrees with its id is either corruption or two different
		// spellings of one principal, and both are identity questions.
		return fmt.Errorf("%w: record name and operator id disagree: the id %q carries name %q", ErrInvalidOperatorRecord, o.OperatorID, name)
	}
	if len(o.AuthPublicKey) != ed25519.PublicKeySize {
		// The length check that must precede every ed25519.Verify (see
		// ErrInvalidPublicKey): Verify PANICS on a wrong-size public key. Doing
		// it here, on the way off disk, is what keeps a truncated record from
		// becoming a panic on an authentication path.
		return fmt.Errorf("%w: operator %q carries a %d-byte auth public key, want exactly %d", ErrInvalidPublicKey, o.OperatorID, len(o.AuthPublicKey), ed25519.PublicKeySize)
	}
	if o.CertFingerprint == ([32]byte{}) {
		// THE ZERO FINGERPRINT IS THE ABSENCE OF A CERTIFICATE AND NAMES NOBODY
		// — certFingerprintOwner's rule, applied one layer earlier because here
		// the certificate is MANDATORY. Storing a zero would create a record
		// that matches every other zero and therefore resolves a connection that
		// presented NOTHING to a definite operator: a fail-OPEN, and the worst
		// one available on this plane.
		return fmt.Errorf("%w: operator %q carries the zero certificate fingerprint, which is the ABSENCE of a certificate rather than a digest; an operator MUST have a client certificate, because invariant 11's cross-check needs a pair to cross-check", ErrInvalidOperatorRecord, o.OperatorID)
	}
	if len(o.Label) > MaxOperatorLabelLen {
		return fmt.Errorf("%w: operator %q carries a %d-byte label, but a label is at most %d; it is not echoed here because it is oversized", ErrInvalidOperatorRecord, o.OperatorID, len(o.Label), MaxOperatorLabelLen)
	}
	if o.CreatedAt.IsZero() {
		return fmt.Errorf("%w: operator %q has a zero created_at", ErrInvalidOperatorRecord, o.OperatorID)
	}
	if o.RevokedAt != nil && o.RevokedAt.IsZero() {
		// An explicitly-set zero revocation time is worse than none: it says
		// "revoked" while carrying no instant. certBindingJSON.RetiredAt refuses
		// the identical shape for the identical reason.
		return fmt.Errorf("%w: operator %q is marked revoked at the zero time; a live operator carries no revoked_at at all", ErrInvalidOperatorRecord, o.OperatorID)
	}
	if o.RevokedReason != "" && o.RevokedAt == nil {
		return fmt.Errorf("%w: operator %q carries a revocation reason but no revoked_at, so it claims to be both live and revoked", ErrInvalidOperatorRecord, o.OperatorID)
	}
	if o.RevokedAt != nil && o.RevokedReason == "" {
		// Attribution is not optional (invariant 6: an operator action must be
		// loudly attributable). A revocation with no reason is a fact nobody can
		// act on later, and the CLI makes -reason required for the same purpose.
		return fmt.Errorf("%w: operator %q is revoked with no reason recorded; a revocation must be attributable", ErrInvalidOperatorRecord, o.OperatorID)
	}
	if len(o.RevokedReason) > MaxOperatorLabelLen {
		return fmt.Errorf("%w: operator %q carries a %d-byte revocation reason, but it is at most %d; it is not echoed here because it is oversized", ErrInvalidOperatorRecord, o.OperatorID, len(o.RevokedReason), MaxOperatorLabelLen)
	}
	return nil
}

// copyOperator returns a deep copy: the public key is freshly allocated and the
// RevokedAt pointer is not shared, so the returned value shares no mutable
// memory with o.
//
// It is the operator mirror of copyRosterEntry and exists for its reason: a
// caller that mutated a backing array would be mutating a STORED CREDENTIAL —
// silently changing which private key authenticates as an ADMIN, or whether that
// admin is still revoked.
func copyOperator(o Operator) Operator {
	out := o
	out.AuthPublicKey = append(ed25519.PublicKey(nil), o.AuthPublicKey...)
	if o.RevokedAt != nil {
		t := *o.RevokedAt
		out.RevokedAt = &t
	}
	return out
}
