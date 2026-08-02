package idem

import (
	"fmt"
	"net/http"
)

// HeaderName is the ONE canonical carrier for a client-supplied idempotency
// key (doc.go point 1). This package defines no body field and no fallback:
// a key that could arrive by two routes would eventually disagree with
// itself.
const HeaderName = "Idempotency-Key"

// MaxKeyLen is the exact byte-length cap on a client-supplied idempotency
// key. See doc.go point 2 for the rationale and for why this intentionally
// matches auth.MaxIdempotencyKeyLen rather than choosing independently.
const MaxKeyLen = 128

// KeyCharset documents, but does not itself enforce, the exact character
// class ValidateKey accepts: ASCII letters, digits, '.', '_' and '-'. See
// doc.go point 2 for why this set and not printable-ASCII-including-space.
const KeyCharset = `[A-Za-z0-9._-]`

// ValidateKey checks the SHAPE of a client-supplied idempotency key: it must
// be non-empty, at most MaxKeyLen bytes, and every byte must be one of
// KeyCharset. It does not know about agents or operations — see
// ValidateIdempotencyKey and Scope for the per-agent, per-operation form
// callers should actually use once an authenticated agent id is available.
//
// The length check runs BEFORE the charset scan and is O(1) (Go strings carry
// their length), so an oversized key is rejected without scanning it — see
// doc.go point 2's "fail-fast" paragraph for why this, combined with the
// header carrier, keeps an oversized key from ever triggering unbounded
// allocation.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: an idempotency key is required on every mutating call so a retry after a lost acknowledgement cannot be applied twice (invariant 10)", ErrMissingKey)
	}
	if len(key) > MaxKeyLen {
		// Not echoed: the key is untrusted and oversized, and an attacker
		// choosing the input must not get to choose a multiple of it back out
		// of a log line (the same discipline ids.ParseAgentID uses for an
		// oversized id).
		return fmt.Errorf("%w: %d bytes, but an idempotency key is at most %d; the key is not echoed here because it is oversized", ErrInvalidKey, len(key), MaxKeyLen)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '-':
		default:
			// The offending BYTE is quoted, never the whole key: the key is
			// untrusted, safe-to-log-length, and about to be written to a log
			// itself.
			return fmt.Errorf("%w: byte %d is %q, but an idempotency key must match %s", ErrInvalidKey, i, key[i:i+1], KeyCharset)
		}
	}
	return nil
}

// FromRequest extracts and validates the idempotency key from an inbound
// request's headers. It is the ONLY function in this package that reads a key
// off the wire, and it never generates one: an absent or empty header returns
// ErrMissingKey, not a minted substitute (doc.go point 5). Callers on a
// mutating route MUST treat both ErrMissingKey and ErrInvalidKey as a 400.
//
// h is a net/http.Header rather than a full *http.Request so this package
// stays independent of any particular router or handler signature; the HTTP
// wiring task (httpapi, out of this task's scope) passes r.Header.
func FromRequest(h http.Header) (string, error) {
	raw := h.Get(HeaderName)
	if raw == "" {
		return "", ErrMissingKey
	}
	if err := ValidateKey(raw); err != nil {
		return "", err
	}
	return raw, nil
}
