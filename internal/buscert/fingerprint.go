package buscert

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"fmt"
)

// DigestSize is the length of a certificate fingerprint in bytes. It is
// sha256.Size and is not a tuning knob: the construction is fixed by the
// ENROL-SHAPE decision and is already durable in records written by
// internal/invite and internal/auth.
const DigestSize = sha256.Size

// Fingerprint identifies ONE certificate: sha256.Sum256(cert.Raw), the digest
// of the leaf's DER encoding.
//
// This is THE fingerprint of the design — the value the invite blob carries and
// the value a client pins (E6, no TOFU). There is deliberately no second
// construction: a digest over the SPKI, or over a PEM encoding, would be a
// different identity for the SAME certificate, and a system with two identities
// for one object eventually compares one against the other and concludes they
// are different buses.
//
// It is a value type, not a slice, so it is comparable, copyable and cannot be
// aliased or resized by a caller.
type Fingerprint [DigestSize]byte

// FingerprintOf returns the fingerprint of a parsed certificate.
//
// It hashes cert.Raw, which is the DER exactly as it arrived or was created —
// NOT a re-encoding of the parsed fields. That matters: re-marshalling a
// certificate is not guaranteed to reproduce the original bytes, and a
// fingerprint that changed on a round trip would fail to match the pin the
// client was given.
func FingerprintOf(cert *x509.Certificate) Fingerprint { return FingerprintOfDER(cert.Raw) }

// FingerprintOfDER returns the fingerprint of a certificate's DER bytes, for
// callers that hold the DER without having parsed it (a handshake peer chain,
// a file just read).
func FingerprintOfDER(der []byte) Fingerprint { return sha256.Sum256(der) }

// ParseFingerprint decodes the textual form: exactly DigestSize*2 LOWERCASE
// hexadecimal characters, with no prefix, no colons and no whitespace.
//
// Uppercase is REJECTED rather than accepted-and-normalised. The textual form
// travels in the invite blob and in log lines, and is compared by eye and by
// naive tooling as often as by this function; permitting two spellings of one
// fingerprint invites a string comparison somewhere else that says two equal
// fingerprints differ.
//
// The returned error matches ErrMalformed. It names no path, because the input
// here is a string rather than a file.
func ParseFingerprint(s string) (Fingerprint, error) {
	var f Fingerprint
	if want := hex.EncodedLen(DigestSize); len(s) != want {
		return f, &certErr{sentinel: ErrMalformed,
			msg: fmt.Sprintf("buscert: a certificate fingerprint is %d lowercase hexadecimal characters, got %d", want, len(s))}
	}
	if _, err := hex.Decode(f[:], []byte(s)); err != nil {
		return Fingerprint{}, &certErr{sentinel: ErrMalformed,
			msg:   "buscert: a certificate fingerprint must be hexadecimal",
			cause: err}
	}
	if f.String() != s {
		// hex.Decode accepts uppercase; the round trip is what rejects it, and
		// it also rejects any other spelling that decodes to the same bytes.
		return Fingerprint{}, &certErr{sentinel: ErrMalformed,
			msg: "buscert: a certificate fingerprint must be LOWERCASE hexadecimal"}
	}
	return f, nil
}

// String renders the fingerprint as lowercase hex — the one textual form.
func (f Fingerprint) String() string { return hex.EncodeToString(f[:]) }

// Equal reports whether two fingerprints are the same.
//
// It uses subtle.ConstantTimeCompare, and the honest reason is NOT that timing
// matters here: a certificate fingerprint is PUBLIC — it is in the invite blob
// and derivable by anyone who completes a handshake — so constant time buys
// nothing against an attacker. It is used because it costs nothing at 32 bytes
// and because the alternative is that the next caller who needs a comparison
// writes a hand-rolled byte loop, and the one after that copies it somewhere it
// IS security-relevant.
func (f Fingerprint) Equal(other Fingerprint) bool {
	return subtle.ConstantTimeCompare(f[:], other[:]) == 1
}
