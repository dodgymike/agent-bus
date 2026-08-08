package attest

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// AttestationFormatVersion is the version of the canonical ATTESTATION signing
// format implemented here. It is a RESERVED number, allocated through the Spec
// Server `signing-format-version` namespace (value 2, reserved 2026-08-08 for
// RELAY-14). Nobody picks one by looking at this constant.
//
// It is a SEPARATE constant from signing.FormatVersion and must stay separate.
// That constant is already consumed as THE format version in a peer-facing
// error string (internal/relay/message.go), so hanging a second, different
// layout off it would put two meanings on one key: a peer told "format version
// 1" would have no way to know which of two byte layouts it had got wrong.
//
// As in internal/signing, the version is not a field of its own — it is spelled
// out inside Context, so there is exactly ONE version indicator in the signed
// bytes and no way for two of them to disagree. Changing ANY of the layout
// below — adding a field, removing one, reordering, changing a width — is a new
// version with a new Context string, never an in-place edit of this one. For a
// FEDERATED artefact that is a flag day across every bus in the federation,
// which is why the layout carries KeyEpoch and IssuedAtUnixMilli even though
// nothing enforces them yet.
const AttestationFormatVersion = 2

// Context is the domain-separation prefix: the first field of every canonical
// attestation. A fixed, documented, length-prefixed ASCII prefix is framing —
// it is not a cryptographic construction, and nothing about the signature
// scheme depends on its content (internal/signing/canonical.go:26-32).
//
// # Disjointness, checked rather than asserted
//
// The first field of an agent-signed MESSAGE and the first field of a
// bus-signed ATTESTATION are:
//
//	msg-sig  0x00000013 || "agent-bus/msg-sig/1"      (19 bytes)
//	attest   0x00000016 || "agent-bus/bus-attest/2"   (22 bytes)
//
// They differ in the LENGTH WORD ITSELF (0x13 vs 0x16), so neither encoding can
// be a prefix of the other and no byte string is a valid encoding of both.
// TestCanonicalIsDisjointFromMessageSigning pins that.
//
// There is a second, stronger and independent separation the message format
// does not have: the two artefacts are signed by DIFFERENT KEYS. A message is
// signed by an AGENT's messaging key; an attestation by the BUS SIGNING key.
// Cross-protocol confusion needs one key that signs both languages, and no such
// key exists.
//
// STANDING REQUIREMENT this creates: the bus signing key signs exactly ONE
// artefact today — this one. A future task that makes it sign a second owns
// re-checking disjointness, the same obligation internal/signing places on
// agent keys.
const Context = "agent-bus/bus-attest/2"

// ErrInvalid wraps every failure of Canonicalize: the attestation is malformed
// and cannot be encoded, so no signature over it can exist to be checked.
//
// It is the "this can never be verified by anyone" case, matching
// internal/relay's taxonomy where such a failure is a 400 rather than a 403 —
// a caller mapping this package's errors onto the wire should answer it as a
// malformed request, not as a refusal to attribute.
//
// That mapping is about material that came FROM A PEER. Sign's failures also
// wrap ErrInvalid, and they are local faults about our own key file and our own
// arguments — but Sign is only ever called by the bus that owns the signing
// key, so no Sign error is ever answered to a peer. The one local fault
// reachable inside Verify has its own sentinel, ErrNoClock, precisely so it
// cannot be reported to a peer as its bad request.
//
// Canonicalize NEVER returns partial or best-effort bytes. Either the input is
// a well-formed attestation and the bytes are exact, or there are no bytes at
// all: signing a guess would produce a signature over something nobody checked.
var ErrInvalid = errors.New("attest: attestation cannot be canonicalized")

// Attestation is the origin bus's signed binding of an agent id to that agent's
// MESSAGING public key. It is the artefact carried VERBATIM in the relay
// envelope from the origin bus to every downstream verifier.
//
// It is a VALUE TYPE, not a pointer, deliberately: a pointer would make any
// verification path reached without a prior shape check a nil dereference — a
// remote panic rather than a refusal, which is the same shape as the
// ed25519.Verify panic trap internal/signing documents. A zero Attestation is
// refused by Verify; it is never a crash.
//
// Provenance of each field, which is the point of the design:
//
//	AgentID             SERVER-minted by the ORIGIN bus ("<bus-id>.<agent-id>")
//	MessagingPublicKey  AGENT-generated, recorded by the ORIGIN bus at enrolment
//	KeyEpoch            ORIGIN-bus-assigned
//	IssuedAtUnixMilli   ORIGIN-bus clock
//	NotAfterUnixMilli   ORIGIN-bus clock + the relay retention window
//	Signature           made by the ORIGIN bus's BUS SIGNING key
//
// Every one of them is the ORIGIN bus's to state. No intermediate may change,
// re-mint or re-attest any of them; an intermediate that does produces a blob
// that fails Verify at the next hop, which is the property the whole design
// exists to obtain.
type Attestation struct {
	// AgentID is the fully-qualified subject, "<bus-id>.<agent-id>"
	// (invariant 2). Its BUS HALF IS the attesting bus: there is deliberately
	// no separate BusID field, because two independent claims about which bus
	// made this statement would force every consumer to choose one of them.
	AgentID string `json:"agent_id"`

	// MessagingPublicKey is the agent's Ed25519 MESSAGING public key — the key
	// that signs that agent's messages (internal/signing), NOT its auth key and
	// NOT any bus key. Exactly ed25519.PublicKeySize bytes.
	MessagingPublicKey ed25519.PublicKey `json:"messaging_public_key"`

	// KeyEpoch is the origin bus's epoch for this binding.
	//
	// IT IS COVERED BY THE SIGNATURE BUT NOT ENFORCED, and that is deliberate.
	// A field the LAYOUT already carries can be enforced later without a format
	// version bump, whereas ADDING one later is a new Context — a
	// federation-wide flag day.
	//
	// DO NOT "improve" this into a monotonicity rule at the verifier. It is the
	// obvious next idea and it is a trap: messages signed under epoch n can
	// legitimately arrive after messages signed under epoch n+1 (two routes,
	// two queue depths), so a "never accept a lower epoch" rule would silently
	// drop legitimate traffic and look exactly like a forgery. Enforcing it
	// needs its own design task, alongside revocation across a non-adjacent
	// link — FEDERATION_TRUST_DEEPDIVE.md §4.4 and its task T11.
	//
	// Zero is accepted: nothing assigns epochs yet, so 0 is the honest "no
	// epoch assigned" value rather than a malformed field.
	KeyEpoch uint64 `json:"key_epoch"`

	// IssuedAtUnixMilli is the origin bus's wall clock at minting: Unix
	// milliseconds UTC as a signed 64-bit integer, encoded fixed-width.
	//
	// Milliseconds rather than nanoseconds so the value is exactly
	// representable as a JSON number, and so the wire envelope carries THIS
	// EXACT INTEGER rather than a formatted timestamp a verifier must re-parse.
	// Every conversion between the wire form and the signed form is a place the
	// two sides can drift, so there is none.
	//
	// It is COVERED BUT NOT ENFORCED. In particular Verify does not reject a
	// future IssuedAt: only the origin bus can mint an attestation for its own
	// agents, and the origin bus already controls attribution of its own agents
	// completely, so a "not yet valid" check would buy nothing and would reject
	// honest traffic under clock skew.
	IssuedAtUnixMilli int64 `json:"issued_at_unix_ms"`

	// NotAfterUnixMilli is when this binding stops being acceptable. Verify
	// ENFORCES it — a MUST, not a SHOULD (see Verify).
	//
	// The value is the MINTER's to choose and this package does not pick one: it
	// must be DERIVED from the maximum relay retention/retry window, never
	// picked as a plausible-sounding constant. The reason is that an
	// intermediate forwards VERBATIM and cannot re-mint (re-minting is
	// re-attestation, the one thing the design forbids), so a message queued at
	// an intermediate across a partition longer than this window becomes
	// permanently undeliverable.
	NotAfterUnixMilli int64 `json:"not_after_unix_ms"`

	// Signature is the detached Ed25519 signature over Canonicalize's output,
	// made with the ORIGIN bus's BUS SIGNING key. Exactly
	// ed25519.SignatureSize bytes. It is NOT part of the canonical bytes.
	Signature []byte `json:"signature"`
}

// Canonicalize returns the exact bytes to be signed and verified for a.
//
// The layout follows internal/signing's normative rules
// (internal/signing/canonical.go:146-162) rather than inventing a parallel set:
// all integers big-endian, every variable-length field preceded by its uint32
// length, a length-prefixed ASCII domain-separation Context as the FIRST field,
// fixed field order. Six fields:
//
//	uint32 len || Context                    ("agent-bus/bus-attest/2")
//	uint32 len || AgentID                    ("<bus-id>.<agent-id>")
//	uint32 len || MessagingPublicKey         (32 raw bytes)
//	uint64        KeyEpoch
//	int64         IssuedAtUnixMilli          (two's complement)
//	int64         NotAfterUnixMilli          (two's complement)
//
// # Why this is UNAMBIGUOUS — the classic failure of a hand-rolled layout
//
// Two distinct field tuples can never produce identical bytes. Field order is
// fixed and known to both ends; three fields are fixed-width; Context is a
// compile-time constant; and the two genuinely variable fields each carry their
// own uint32 length AHEAD of their bytes, so a decoder consumes exactly the
// bytes the encoder wrote and no boundary can be read in two places. There is
// no separator character whose absence from the alphabet has to be argued (the
// pre-existing ad-hoc test encoding in internal/relay/signed_test.go uses '|'
// and is unambiguous only by accident), and there is no field whose length is
// implied by the total.
//
// TestCanonicalEncodingIsUnambiguous exercises that as a property: every
// single-field perturbation of a fixed input produces different bytes.
//
// # The output is handed to ed25519.Sign / ed25519.Verify UNHASHED
//
// Do NOT pre-hash it. Ed25519 signs the message, not a digest of it;
// crypto/ed25519 exposes no pre-hash mode, and handing it a digest produces a
// valid-looking signature over 32 bytes that no conforming verifier will ever
// reproduce. That is the silent-failure shape invariant 9 exists for — it would
// still "sign", it would simply protect nothing anyone checks.
//
// Signature is NOT covered: a signature cannot cover itself.
func Canonicalize(a Attestation) ([]byte, error) {
	if err := validate(a); err != nil {
		return nil, err
	}

	size := 4 + len(Context) +
		4 + len(a.AgentID) +
		4 + len(a.MessagingPublicKey) +
		8 + 8 + 8

	out := make([]byte, 0, size)
	out = appendLenPrefixed(out, []byte(Context))
	out = appendLenPrefixed(out, []byte(a.AgentID))
	out = appendLenPrefixed(out, a.MessagingPublicKey)
	out = appendUint64(out, a.KeyEpoch)
	// The timestamps are encoded as two's-complement int64s — they are signed in
	// the LAYOUT so the format never needs a version bump to admit a pre-1970
	// clock. validate() nevertheless rejects a non-positive value as an unset
	// field, so those branches of the encoding are a property of the format
	// rather than a reachable case.
	out = appendUint64(out, uint64(a.IssuedAtUnixMilli))
	out = appendUint64(out, uint64(a.NotAfterUnixMilli))
	return out, nil
}

// validate checks every covered field. It fails closed: an unparseable agent
// id, a wrong-sized key, or an unset timestamp stops the encoding rather than
// producing bytes.
//
// It deliberately does NOT check Signature — Canonicalize produces the bytes a
// signature is made over, and requiring the signature to exist first would make
// minting impossible.
func validate(a Attestation) error {
	// ids.ParseAgentID BOUNDS the id before anything quotes it (it refuses an
	// oversized id without echoing it), so every error below that quotes
	// a.AgentID quotes a value already known to be at most ids.MaxAgentIDLen
	// bytes drawn from a restricted alphabet. An attestation arrives from a
	// peer, so the id is attacker-chosen, and %q expands a control byte to four
	// characters: an unbounded value would let a peer choose the size of the
	// line we log about refusing it.
	if _, _, _, err := ids.ParseAgentID(a.AgentID); err != nil {
		return fmt.Errorf("%w: agent id: %v", ErrInvalid, err)
	}

	// Checked with internal/signing's exported check rather than a second copy
	// of "is it 32 bytes". A control that exists in two places is a control
	// that drifts in one of them — and this one is the ed25519.Verify panic
	// trap, so it is a remote-DoS guard rather than a tidiness one.
	if err := signing.ValidatePublicKey(a.MessagingPublicKey); err != nil {
		return fmt.Errorf("%w: messaging public key: %v", ErrInvalid, err)
	}

	if a.IssuedAtUnixMilli <= 0 {
		return fmt.Errorf("%w: issued-at %d is not a positive Unix millisecond value; 0 means \"unset\"", ErrInvalid, a.IssuedAtUnixMilli)
	}
	if a.NotAfterUnixMilli <= 0 {
		return fmt.Errorf("%w: not-after %d is not a positive Unix millisecond value; 0 means \"unset\", and an attestation with no expiry is exactly what expiry-as-a-MUST forbids", ErrInvalid, a.NotAfterUnixMilli)
	}
	if a.NotAfterUnixMilli <= a.IssuedAtUnixMilli {
		// Refused at the ENCODER so a minting bus learns immediately, rather
		// than at 3am when every message it has ever relayed is refused as
		// expired by a peer.
		return fmt.Errorf("%w: not-after %d is not after issued-at %d; an attestation that expires before it is issued is never valid", ErrInvalid, a.NotAfterUnixMilli, a.IssuedAtUnixMilli)
	}
	return nil
}

// appendLenPrefixed appends a uint32 big-endian length followed by b. validate
// has already bounded every field, so a length that does not fit in a uint32
// cannot reach here; the check stays because a silently truncated length would
// produce bytes that encode a DIFFERENT attestation.
func appendLenPrefixed(dst, b []byte) []byte {
	if uint64(len(b)) > math.MaxUint32 {
		panic("attest: field longer than uint32 reached the encoder; validate() must reject it first")
	}
	dst = appendUint32(dst, uint32(len(b)))
	return append(dst, b...)
}

func appendUint32(dst []byte, v uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	return append(dst, buf[:]...)
}

func appendUint64(dst []byte, v uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	return append(dst, buf[:]...)
}
