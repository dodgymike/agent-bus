package invite

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
)

// RecordKind is the wal.Entry.Kind discriminator for an invite record.
//
// Entry.Kind is a FREE-FORM APPLICATION DISCRIMINATOR (internal/wal/log.go), not
// a numbered frame type: it sits inside the prepare payload, above the framing
// layer. So NO record-type reservation is needed for it and
// internal/wal/format.go's Type enum is not touched. This is written down so
// that the next reader does not go and reserve a number that nothing requires.
const RecordKind = "invite"

// State is where an invite is in its lifecycle. It is a CLOSED enum: a value
// outside these three is rejected by Record.validate in both directions.
type State uint8

const (
	// StateOpen is a minted, unspent, unrevoked invite. Whether it is still
	// USABLE additionally depends on the clock — see Record.Expired.
	StateOpen State = iota + 1
	// StateRedeemed is a SPENT invite. Terminal. Single use is exhausted.
	StateRedeemed
	// StateRevoked is an invite an operator withdrew before it was redeemed.
	// Terminal.
	StateRevoked
)

// "EXPIRED" IS DELIBERATELY NOT A STATE.
//
// Expiry is a pure predicate over ExpiresAt and the current clock, evaluated on
// every call (Record.Expired), and never a stored flag. A stored flag is a
// snapshot of one clock reading that starts disagreeing with the clock the
// instant it is written, and keeping it true would mean a background sweep
// REWRITING records into an append-only log. That is the same reasoning
// internal/idem applies to CommittedAt: derive the predicate identically on the
// live path and the replay path, and memory and disk can never disagree about
// which records are live.

// String returns the wire spelling of the state. It is the value that goes on
// disk, so it is a fixed string and not a number: a numeric enum in a durable
// record is unreadable to the operator who has to interpret the log with `head
// -c` and a pretty-printer, and it silently changes meaning if the constants
// are ever reordered.
func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateRedeemed:
		return "redeemed"
	case StateRevoked:
		return "revoked"
	default:
		return fmt.Sprintf("State(%d)", uint8(s))
	}
}

// parseState maps the wire spelling back onto a State. An unrecognised value is
// an error, never a default: guessing here would turn a corrupt or
// future-format record into a plausible-looking open invite.
func parseState(s string) (State, error) {
	switch s {
	case "open":
		return StateOpen, nil
	case "redeemed":
		return StateRedeemed, nil
	case "revoked":
		return StateRevoked, nil
	default:
		return 0, fmt.Errorf("%w: state %q is not one of open, redeemed, revoked", ErrInvalidRecord, elide(s))
	}
}

// Record is one invite: the server-minted id, the bus it admits to, the DIGEST
// of its bearer secret, its validity window, and the terminal event that spent
// it, if any.
//
// EVERY DURABLE ENTRY CARRIES THE COMPLETE RECORD IN ITS POST-TRANSITION STATE,
// never a delta. Two reasons, both load-bearing:
//
//   - replay needs no ordering logic beyond a monotonic upsert, so there is no
//     second mechanism that could disagree with the live path;
//   - if an EARLIER record for the same invite is discarded by recovery (a
//     corrupt frame, a capacity discard), a surviving LATER record still
//     reconstructs the invite in its SPENT state. Under a delta scheme the same
//     loss would leave the invite looking OPEN — the one direction that produces
//     a second redemption.
//
// The plaintext secret is NOT here and never will be. See secret.go.
type Record struct {
	// ID is the server-minted invite id (invariant 1). A client-supplied id is
	// input to be validated, never an identity to be trusted.
	ID string

	// BusID is the bus this invite admits an agent to. It is part of the record
	// because the invite blob an agent receives names a bus, an address and a
	// certificate fingerprint (DECISIONS.md, E6) — an invite that did not say
	// which bus it belonged to could be presented to a different one.
	BusID string

	// SecretDigest is HashSecret(secret). The secret itself is returned exactly
	// once, by Mint, and is never stored, logged or written to the WAL.
	SecretDigest [DigestSize]byte

	// Label is an optional operator note (why this invite was minted, for whom).
	// At most MaxLabelLen bytes.
	//
	// It is OPERATOR TEXT and must NEVER be echoed to a client: it may name a
	// person, a team or a ticket, and the party redeeming an invite is by
	// definition not yet authenticated.
	Label string

	// CreatedAt is when the invite was minted.
	CreatedAt time.Time

	// ExpiresAt is when it stops being redeemable. Expiry is evaluated against
	// this field and the clock — see Record.Expired and the note above about why
	// there is no "expired" state.
	ExpiresAt time.Time

	// State is the lifecycle state. The fields below are valid if and only if
	// State names their event; validate enforces that in both directions.
	State State

	// RedeemedAt is when the redemption committed. Set iff State ==
	// StateRedeemed. It is ALSO the input to spent-record retention, so a
	// redeemed record without one could never be dropped.
	RedeemedAt time.Time

	// RedeemedBy is the fully-qualified "<bus-id>.<agent-id>" (invariant 2) the
	// redemption created. It is the provenance that answers "who did this invite
	// let onto the bus".
	RedeemedBy string

	// RedeemKey is the CLIENT IDEMPOTENCY KEY the redemption carried, scoped to
	// THIS invite (see doc.go on the idempotency scope). Comparing it is the
	// first half of separating a legitimate retry from key reuse.
	RedeemKey string

	// RedeemFingerprint is the payload fingerprint of the redemption request.
	// Comparing it is the second half: same key + SAME fingerprint is a
	// legitimate retry that gets the original Result back; same key + DIFFERENT
	// fingerprint is a protocol violation (ErrKeyReuse).
	RedeemFingerprint idem.Fingerprint

	// Result is the minted redemption result, verbatim, as the route returned
	// it. It is opaque to this package and is capped at idem.MaxResultBytes.
	// Storing it — rather than merely remembering that the key was used — is
	// what makes it possible to ANSWER a retry instead of only refusing it.
	Result json.RawMessage

	// CertFingerprint is sha256.Sum256(cert.Raw) of the client certificate bound
	// at redemption. The name and the exact input are fixed by the ENROL-SHAPE
	// decision (DECISIONS.md, 2026-08-07) so that nobody invents a second,
	// incompatible fingerprint for the same certificate.
	//
	// IT IS DEFINED BUT UNUSED, DELIBERATELY, FROM DAY ONE. Nothing in this
	// package validates it beyond "it is DigestSize bytes, or absent", and the
	// ZERO value means "no certificate was bound" — which is the only value
	// anything writes today. It is here now so that MTLS-BIND adds a CHECK
	// rather than a schema change to records that are already durable.
	CertFingerprint [DigestSize]byte

	// RevokedAt is when an operator revoked the invite. Set iff State ==
	// StateRevoked.
	RevokedAt time.Time

	// RevokedReason is an optional operator note, at most MaxReasonLen bytes.
	// Operator text, like Label: never echoed to a client.
	RevokedReason string
}

// recordJSON is the wire shape: compact, no HTML escaping, omitempty on
// everything optional, digests hex-encoded (matching idem's "fp") and times
// RFC3339Nano in UTC.
//
// The state is a fixed STRING and the digests are HEX so that an operator can
// read a record straight out of the WAL with `head -c` and a pretty-printer —
// the property internal/wal chose a JSON payload for in the first place.
type recordJSON struct {
	ID                string          `json:"id"`
	BusID             string          `json:"bus"`
	SecretDigest      string          `json:"secret_sha256"`
	Label             string          `json:"label,omitempty"`
	CreatedAt         string          `json:"created_at"`
	ExpiresAt         string          `json:"expires_at"`
	State             string          `json:"state"`
	RedeemedAt        string          `json:"redeemed_at,omitempty"`
	RedeemedBy        string          `json:"redeemed_by,omitempty"`
	RedeemKey         string          `json:"redeem_key,omitempty"`
	RedeemFingerprint string          `json:"redeem_fp,omitempty"`
	Result            json.RawMessage `json:"result,omitempty"`
	CertFingerprint   string          `json:"cert_sha256,omitempty"`
	RevokedAt         string          `json:"revoked_at,omitempty"`
	RevokedReason     string          `json:"revoked_reason,omitempty"`
}

// Expired reports whether the invite's validity window has closed.
//
// It is a PURE PREDICATE and the ONLY definition of expiry in this package —
// the live path and the replay path both call it, so they cannot drift into
// disagreeing about which invites are still live.
//
// It says nothing about State: a REDEEMED record whose ExpiresAt has passed is
// still retained (for SpentRetention) and still answers a legitimate retry,
// because the retry horizon is deliberately longer than the invite's lifetime.
// Expiry gates a FRESH redemption only.
func (r Record) Expired(now time.Time) bool { return now.After(r.ExpiresAt) }

// zeroDigest is the all-zero digest, used to test "absent" for the two
// fixed-size digest fields.
var zeroDigest [DigestSize]byte

// validate checks a Record is self-consistent.
//
// IT RUNS IN BOTH DIRECTIONS, and both matter for different reasons. On the way
// OUT (Encode, before the durable write) a record that cannot be stored fails
// the operation with NOTHING written, rather than being discovered at replay
// when the effect is already durable and every remaining option is bad. On the
// way IN (DecodeRecord) a record read off disk is UNTRUSTED INPUT (invariant 1)
// even though this server wrote it — because "this server wrote it" is exactly
// the claim corruption disproves.
//
// Every bounded field is checked here, not merely described in retention.go: an
// unenforced bound would make MaxRecordBytes a description of the happy path,
// and a record off a damaged or hostile log could then carry a field limited
// only by wal.MaxPayloadSize (1 MiB).
func (r Record) validate() error {
	if err := ValidateInviteID(r.ID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if r.BusID == "" {
		return fmt.Errorf("%w: an invite record must name the bus it admits to", ErrInvalidRecord)
	}
	if len(r.BusID) > MaxBusIDLen {
		return fmt.Errorf("%w: bus id is %d bytes, but a record's bus id is at most %d; it is not echoed here because it is oversized", ErrInvalidRecord, len(r.BusID), MaxBusIDLen)
	}
	if r.SecretDigest == zeroDigest {
		// An all-zero digest is either an uninitialised field or corruption. It
		// is refused rather than stored: an invite nothing can ever redeem is
		// dead weight inside a bounded table, and the far worse reading — that
		// the digest was simply never set — must not be allowed to look normal.
		return fmt.Errorf("%w: the secret digest is all zero, which is either an uninitialised field or corruption", ErrInvalidRecord)
	}
	if len(r.Label) > MaxLabelLen {
		return fmt.Errorf("%w: label is %d bytes, but a label is at most %d; it is not echoed here because it is oversized", ErrInvalidRecord, len(r.Label), MaxLabelLen)
	}
	if r.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at is the zero time", ErrInvalidRecord)
	}
	if r.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: expires_at is the zero time, but expiry is computed from it, so a record without one could never expire", ErrInvalidRecord)
	}
	if !r.ExpiresAt.After(r.CreatedAt) {
		return fmt.Errorf("%w: expires_at (%s) is not after created_at (%s), so the invite was never valid", ErrInvalidRecord, r.ExpiresAt.UTC().Format(time.RFC3339Nano), r.CreatedAt.UTC().Format(time.RFC3339Nano))
	}
	switch r.State {
	case StateOpen:
		// An OPEN record carries NO terminal fields at all. Checked field by
		// field rather than trusted: a record that said "open" while carrying a
		// redemption would be exactly the shape a resurrection attack wants.
		if err := r.mustHaveNoRedemption(); err != nil {
			return err
		}
		if err := r.mustHaveNoRevocation(); err != nil {
			return err
		}
	case StateRedeemed:
		if r.RedeemedAt.IsZero() {
			return fmt.Errorf("%w: a redeemed invite must record redeemed_at, and spent-record retention is computed from it", ErrInvalidRecord)
		}
		if r.RedeemedBy == "" {
			return fmt.Errorf("%w: a redeemed invite must name the agent id it created", ErrInvalidRecord)
		}
		if len(r.RedeemedBy) > idem.MaxAgentLen {
			return fmt.Errorf("%w: redeemed_by is %d bytes, but an agent id is at most %d; it is not echoed here because it is oversized", ErrInvalidRecord, len(r.RedeemedBy), idem.MaxAgentLen)
		}
		if err := idem.ValidateKey(r.RedeemKey); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRecord, err)
		}
		if r.RedeemFingerprint == (idem.Fingerprint{}) {
			// A zero fingerprint is not merely untidy, it is EXPLOITABLE, which
			// is why it is refused rather than tolerated as an absent optional.
			// Store.Begin's triage treats a matching fingerprint as a legitimate
			// retry and hands back the ORIGINAL RESULT; a stored zero would
			// therefore match a request that carries no fingerprint at all and
			// replay an agent identity to it, where the correct answer is
			// ErrKeyReuse. Encode always writes the full 64 hex characters, so
			// no record this package produces can reach here — the case that can
			// is a record whose "redeem_fp" was dropped or zeroed on disk, which
			// is precisely the untrusted input validate exists to catch
			// (mustHaveNoRedemption already checks the field for every other
			// state; this closes the one state that did not).
			return fmt.Errorf("%w: a redeemed invite must record the payload fingerprint of the redemption that spent it; an all-zero fingerprint would make a request carrying no fingerprint look like a legitimate retry", ErrInvalidRecord)
		}
		if len(r.Result) > idem.MaxResultBytes {
			return fmt.Errorf("%w: %d bytes, but a stored result is at most %d; the result is not echoed here because it is oversized", ErrResultTooLarge, len(r.Result), idem.MaxResultBytes)
		}
		if err := r.mustHaveNoRevocation(); err != nil {
			return err
		}
	case StateRevoked:
		if r.RevokedAt.IsZero() {
			return fmt.Errorf("%w: a revoked invite must record revoked_at", ErrInvalidRecord)
		}
		if len(r.RevokedReason) > MaxReasonLen {
			return fmt.Errorf("%w: revoked_reason is %d bytes, but a reason is at most %d; it is not echoed here because it is oversized", ErrInvalidRecord, len(r.RevokedReason), MaxReasonLen)
		}
		if err := r.mustHaveNoRedemption(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: %s is not one of the fixed lifecycle states", ErrInvalidRecord, r.State)
	}
	return nil
}

// mustHaveNoRedemption enforces "these fields exist only on a redeemed record".
func (r Record) mustHaveNoRedemption() error {
	switch {
	case !r.RedeemedAt.IsZero():
		return fmt.Errorf("%w: a %s invite carries a redeemed_at", ErrInvalidRecord, r.State)
	case r.RedeemedBy != "":
		// Not echoed: on a record that should not have one it is either
		// corruption or an injected value, and quoting it back puts
		// attacker-chosen text into an operator's log.
		return fmt.Errorf("%w: a %s invite carries a redeemed_by (%d bytes)", ErrInvalidRecord, r.State, len(r.RedeemedBy))
	case r.RedeemKey != "":
		return fmt.Errorf("%w: a %s invite carries a redeem_key (%d bytes)", ErrInvalidRecord, r.State, len(r.RedeemKey))
	case r.RedeemFingerprint != (idem.Fingerprint{}):
		return fmt.Errorf("%w: a %s invite carries a redeem fingerprint", ErrInvalidRecord, r.State)
	case len(r.Result) != 0:
		return fmt.Errorf("%w: a %s invite carries a stored result (%d bytes)", ErrInvalidRecord, r.State, len(r.Result))
	case r.CertFingerprint != zeroDigest:
		return fmt.Errorf("%w: a %s invite carries a client certificate fingerprint, which is bound only at redemption", ErrInvalidRecord, r.State)
	}
	return nil
}

// mustHaveNoRevocation enforces "these fields exist only on a revoked record".
func (r Record) mustHaveNoRevocation() error {
	switch {
	case !r.RevokedAt.IsZero():
		return fmt.Errorf("%w: a %s invite carries a revoked_at", ErrInvalidRecord, r.State)
	case r.RevokedReason != "":
		return fmt.Errorf("%w: a %s invite carries a revoked_reason (%d bytes)", ErrInvalidRecord, r.State, len(r.RevokedReason))
	}
	return nil
}

// Encode renders the record as the opaque JSON that rides in wal.Entry.Body.
//
// IT VALIDATES BEFORE IT RETURNS, so a record that cannot be stored fails the
// operation with nothing written. See validate.
func (r Record) Encode() (json.RawMessage, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	result, err := canonicalResult(r.Result)
	if err != nil {
		return nil, err
	}
	j := recordJSON{
		ID:           r.ID,
		BusID:        r.BusID,
		SecretDigest: hex.EncodeToString(r.SecretDigest[:]),
		Label:        r.Label,
		CreatedAt:    r.CreatedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt:    r.ExpiresAt.UTC().Format(time.RFC3339Nano),
		State:        r.State.String(),
		Result:       result,
	}
	// The terminal fields are written ONLY for the state that owns them, so the
	// encoder cannot produce a record its own validate would refuse on the way
	// back in.
	if r.State == StateRedeemed {
		j.RedeemedAt = r.RedeemedAt.UTC().Format(time.RFC3339Nano)
		j.RedeemedBy = r.RedeemedBy
		j.RedeemKey = r.RedeemKey
		j.RedeemFingerprint = hex.EncodeToString(r.RedeemFingerprint[:])
		if r.CertFingerprint != zeroDigest {
			j.CertFingerprint = hex.EncodeToString(r.CertFingerprint[:])
		}
	}
	if r.State == StateRevoked {
		j.RevokedAt = r.RevokedAt.UTC().Format(time.RFC3339Nano)
		j.RevokedReason = r.RevokedReason
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(j); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	// Encoder.Encode terminates with a newline; the carrier is length-delimited
	// and needs no terminator.
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// canonicalResult compacts a stored result so that a live Apply and a replayed
// Apply hold BYTE-IDENTICAL bytes — the same discipline wal.canonicalBody
// applies to an entry body. A result of literal null normalises to absent.
func canonicalResult(result json.RawMessage) (json.RawMessage, error) {
	if len(result) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, result); err != nil {
		return nil, fmt.Errorf("%w: result is not valid JSON: %v", ErrInvalidRecord, err)
	}
	if buf.String() == "null" {
		return nil, nil
	}
	out := json.RawMessage(buf.Bytes())
	if len(out) > idem.MaxResultBytes {
		return nil, fmt.Errorf("%w: %d bytes, but a stored result is at most %d; the result is not echoed here because it is oversized", ErrResultTooLarge, len(out), idem.MaxResultBytes)
	}
	return out, nil
}

// DecodeRecord parses an invite record read back off disk.
//
// It is STRICT in exactly the way wal.decodePayload and idem.DecodeRecord are:
// unknown fields are refused, trailing data is refused, and every field is
// re-validated. A lenient decoder here would reinstate an invite with a mangled
// state, a mangled digest or a validity window that never closes — and the
// worst of those failures reinstates a SPENT invite as an open one.
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
	state, err := parseState(j.State)
	if err != nil {
		return Record{}, err
	}
	createdAt, err := parseTime("created_at", j.CreatedAt)
	if err != nil {
		return Record{}, err
	}
	expiresAt, err := parseTime("expires_at", j.ExpiresAt)
	if err != nil {
		return Record{}, err
	}
	r := Record{
		ID:            j.ID,
		BusID:         j.BusID,
		Label:         j.Label,
		CreatedAt:     createdAt,
		ExpiresAt:     expiresAt,
		State:         state,
		RedeemedBy:    j.RedeemedBy,
		RedeemKey:     j.RedeemKey,
		Result:        j.Result,
		RevokedReason: j.RevokedReason,
	}
	if err := parseDigest("secret_sha256", j.SecretDigest, r.SecretDigest[:]); err != nil {
		return Record{}, err
	}
	if j.CertFingerprint != "" {
		if err := parseDigest("cert_sha256", j.CertFingerprint, r.CertFingerprint[:]); err != nil {
			return Record{}, err
		}
	}
	if j.RedeemFingerprint != "" {
		if err := parseDigest("redeem_fp", j.RedeemFingerprint, r.RedeemFingerprint[:]); err != nil {
			return Record{}, err
		}
	}
	if j.RedeemedAt != "" {
		if r.RedeemedAt, err = parseTime("redeemed_at", j.RedeemedAt); err != nil {
			return Record{}, err
		}
	}
	if j.RevokedAt != "" {
		if r.RevokedAt, err = parseTime("revoked_at", j.RevokedAt); err != nil {
			return Record{}, err
		}
	}
	if err := r.validate(); err != nil {
		return Record{}, err
	}
	return r, nil
}

// parseTime decodes one RFC3339Nano timestamp, normalised to UTC.
func parseTime(field, v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		// The value is quoted through elide: it is file-derived text of
		// unbounded length until validate has run.
		return time.Time{}, fmt.Errorf("%w: %s (%q) is not RFC3339Nano: %v", ErrInvalidRecord, field, elide(v), err)
	}
	return t.UTC(), nil
}

// parseDigest decodes one hex digest into dst, which must be DigestSize bytes.
func parseDigest(field, v string, dst []byte) error {
	b, err := hex.DecodeString(v)
	if err != nil {
		return fmt.Errorf("%w: %s is not hex: %v", ErrInvalidRecord, field, err)
	}
	if len(b) != len(dst) {
		return fmt.Errorf("%w: %s is %d bytes, want %d", ErrInvalidRecord, field, len(b), len(dst))
	}
	copy(dst, b)
	return nil
}

// maxElidedChars bounds how much untrusted, file-derived text may appear in an
// error string. The same discipline wal's CorruptError applies: an operator's
// log must not be sizeable by whoever wrote the damaged bytes.
const maxElidedChars = 64

// elide truncates untrusted text for inclusion in an error message.
func elide(s string) string {
	if len(s) <= maxElidedChars {
		return s
	}
	return s[:maxElidedChars] + "…(truncated)"
}

// copyRecord returns a deep copy: the Result slice is freshly allocated, so the
// returned record shares no mutable memory with r.
//
// It is applied on the way IN and on the way OUT of the table, the same
// discipline auth.copyRosterEntry uses, and for the same reason: a caller that
// still holds — or is later handed — the backing array of a stored record could
// mutate the durable serving copy of a credential-bearing record without going
// anywhere near the write path.
func copyRecord(r Record) Record {
	out := r
	if len(r.Result) > 0 {
		out.Result = append(json.RawMessage(nil), r.Result...)
	} else {
		// Normalised to nil so an empty-but-non-nil result and an absent one are
		// the same value, and a round trip through Encode/DecodeRecord cannot
		// change it.
		out.Result = nil
	}
	return out
}
