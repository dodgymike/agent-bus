package idem

import (
	"crypto/sha256"
	"encoding/binary"
)

// FingerprintSize is the length in bytes of a Fingerprint (crypto/sha256's
// digest size).
const FingerprintSize = sha256.Size

// Fingerprint is the canonical payload fingerprint IDEM-11's applied-key
// store pins next to a Scope (doc.go point 8): IDEM-12's legitimate-retry
// check and IDEM-14's key-reuse-different-payload check both turn on
// comparing two Fingerprints for equality.
type Fingerprint [FingerprintSize]byte

// ComputeFingerprint hashes fields with crypto/sha256 (stdlib; a plain content
// hash used only for equality comparison, not a MAC or signature, so this is
// not the hand-rolled-crypto CLAUDE.md forbids — see doc.go point 8).
//
// Each field is length-prefixed (an 8-byte big-endian length, then the bytes)
// before being written to the hash, so field boundaries can never be
// ambiguous: ComputeFingerprint([]byte("ab"), []byte("c")) and
// ComputeFingerprint([]byte("a"), []byte("bc")) hash to DIFFERENT digests
// even though naive concatenation would make them equal. Every call site
// must document, at the call site, the fixed field list and order it passes
// — that per-operation list is IDEM-11's and the route tasks' to define, not
// this one's.
func ComputeFingerprint(fields ...[]byte) Fingerprint {
	h := sha256.New()
	var lenBuf [8]byte
	for _, f := range fields {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(f)))
		h.Write(lenBuf[:])
		h.Write(f)
	}
	var out Fingerprint
	copy(out[:], h.Sum(nil))
	return out
}
