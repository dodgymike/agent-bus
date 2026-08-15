package invite

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// SecretBytes is the amount of crypto/rand entropy in an invite secret: 32
// bytes, the same as auth's session token. The secret is a BEARER CREDENTIAL —
// whoever holds it can enrol an agent onto this bus — so it is sized and
// treated exactly like one.
const SecretBytes = 32

// DigestSize is the length of a stored secret digest and of the client
// certificate fingerprint (crypto/sha256's digest size, 32 bytes).
const DigestSize = sha256.Size

// EncodedSecretLen is the length of the base64.RawURLEncoding form
// GenerateSecret returns: 43 characters for 32 bytes.
var EncodedSecretLen = base64.RawURLEncoding.EncodedLen(SecretBytes)

// MaxSecretLen bounds a PRESENTED secret — untrusted input arriving from a
// client — at 256 bytes.
//
// It is generous rather than exact (a secret this bus minted is
// EncodedSecretLen bytes) so that a future change to SecretBytes or to the
// encoding does not silently invalidate live invites. What it is for is
// bounding the work an unauthenticated caller can make this bus do: without it,
// a redemption attempt could hand a megabyte to SHA-256 on a pre-auth route.
// A presented secret over this length cannot be one this bus minted, so
// refusing it costs nothing.
const MaxSecretLen = 256

// GenerateSecret mints an invite secret: SecretBytes from crypto/rand, encoded
// with base64.RawURLEncoding so it is safe in an HTTP header, a URL, a shell
// variable and a JSON blob with no escaping — the identical reasoning
// auth.newToken records for the session token.
//
// A crypto/rand failure is a HARD ERROR with no weaker fallback: a predictable
// invite secret is a forgeable admission ticket to the bus.
//
// THE PLAINTEXT IS RETURNED EXACTLY ONCE, BY Store.Mint, AND IS NEVER STORED.
// It is not in the Record, not in the WAL, and must never appear in a log line
// or an error string — not even a prefix of it. Only HashSecret's digest is
// durable.
func GenerateSecret() (string, error) {
	b := make([]byte, SecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("invite: reading %d bytes from crypto/rand for an invite secret: %w; there is no weaker fallback, because a predictable secret is a forgeable admission ticket", SecretBytes, err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashSecret returns the SHA-256 digest of a secret. This is the ONLY form of
// the secret that is ever stored or written to disk.
//
// # Why a plain SHA-256 and NOT a password hash
//
// The input is SecretBytes of crypto/rand, so there is no low-entropy guess
// space for a slow hash to slow down: an attacker holding the digests has
// nothing to iterate over. This is the identical reasoning
// internal/auth/session.go's tokenHash already records for the session table,
// and the two must read as the same decision.
//
// It is also NOT hand-rolled crypto (invariant 9): it is a stdlib hash used for
// its intended purpose — a one-way digest of a full-entropy value — with no
// bespoke construction, no invented padding and no assembled primitives.
func HashSecret(secret string) [DigestSize]byte {
	return sha256.Sum256([]byte(secret))
}

// VerifySecret reports whether presented is the secret behind stored.
//
// It ALWAYS hashes first and then compares with crypto/subtle.
// ConstantTimeCompare. Hashing first is what keeps a length difference from
// leaking through the comparison at all: two digests are always DigestSize
// bytes, whatever the presented secret's length, so the comparison performs
// identical work for a one-byte guess and a correct 43-byte secret.
//
// # What this does and does NOT buy, stated honestly
//
// It removes the COMPARISON oracle. It does not remove every oracle: a map
// lookup for an unknown invite id takes a different path from a found one, and
// this package's distinct sentinels (errors.go) tell an operator — and
// therefore, if a handler is careless, a client — which failure occurred.
// Store.Begin narrows the first by comparing against a per-store dummy digest
// for an unknown id, so the hash-and-compare work happens either way.
//
// THE SECOND HAS LANDED, at the HTTP boundary (INVITE-HARDEN, 2026-08-14):
// httpapi.writeInviteError maps ErrUnknownInvite, ErrExpired, ErrRevoked,
// ErrAlreadyRedeemed and ErrInvalidInviteID to ONE status and ONE body, logging
// the specific sentinel server-side only. The obligation sits on the boundary,
// not on this package, so a SECOND handler built on these sentinels owes the
// same collapse — see errors.go.
func VerifySecret(presented string, stored [DigestSize]byte) bool {
	got := HashSecret(presented)
	return subtle.ConstantTimeCompare(got[:], stored[:]) == 1
}
