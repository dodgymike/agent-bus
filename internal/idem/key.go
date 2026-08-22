package idem

import (
	"fmt"
	"net/http"
)

// HeaderName is the canonical carrier for a client-supplied idempotency key on
// the BUS-TO-BUS (relay and roster) plane — doc.go point 1 describes the intent,
// and this comment records what actually shipped.
//
// NARROWED 2026-08-21 (IDEM-18). It was written as "the ONE canonical carrier"
// for every mutating call, and that is not what the code does: the only
// production readers and writers of this header are in internal/relay
// (relayhttp.go, rosterhttp.go, handshake.go, client.go). No internal/httpapi
// HANDLER reads it. That package does MOUNT /v1/peer/enroll, /v1/peer/relay and
// /v1/peer/roster (peermount.go), but their handlers are internal/relay's, so
// those routes are this same bus-to-bus plane and not an exception to it. On the
// AGENT-facing plane the key arrives instead as the
// `idempotency_key` JSON body field (httpapi.SendRequestBody,
// BroadcastRequestBody, MintRequestBody and EnrolRequestBody), which is what
// cmd/agent-busctl and the client package send.
//
// This is deliberately recorded rather than silently corrected: an implementer
// who trusted the old wording would set a header /v1/send ignores, and then see
// a 400 for a missing key it believes it supplied. One key still never arrives
// by two routes on the same plane — the relay surface reads only this header,
// the agent surface reads only the body field — which is the property the
// original "no fallback" sentence was protecting.
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
// wiring task (httpapi, out of this task's scope) was expected to pass r.Header.
//
// IT NEVER DID, AND THIS FUNCTION HAS ZERO PRODUCTION CALLERS (stated plainly
// 2026-08-21, IDEM-18; verified by grep — the only callers are in
// internal/idem/idem_test.go). httpapi chose the `idempotency_key` JSON body
// field instead (see HeaderName above), and internal/relay reads the header
// directly with r.Header.Get(idem.HeaderName) rather than through this helper.
// So the sentence above describes a plan, not a live path. It is left standing,
// with this correction beside it, because the paragraph explains WHY the
// signature is http.Header — and removing the function is a code change beyond
// the documentation task that found this, filed as a follow-up instead.
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
