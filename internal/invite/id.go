package invite

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"regexp"
	"strings"
)

// InviteIDPattern is the ONE definition of a legal invite id: the fixed "inv-"
// prefix followed by 16 to 32 lowercase base32 characters (RFC 4648's alphabet
// is A-Z2-7; lowercased it is a-z2-7).
//
// '.' is excluded for the same reason ids.BusIDPattern excludes it — invariant
// 2 qualifies agents as "<bus-id>.<agent-id>" and a '.' inside any id would
// make that qualification ambiguous. The range is 16..32 rather than a single
// exact length so the entropy can be raised later without invalidating ids
// already minted; GenerateInviteID emits exactly 16 today.
const InviteIDPattern = `^inv-[a-z2-7]{16,32}$`

var inviteIDRegexp = regexp.MustCompile(InviteIDPattern)

// MaxInviteIDLen is the hard byte cap on an invite id, checked BEFORE the
// pattern. The longest id the pattern admits is 4+32 = 36 bytes, so 64 is
// deliberately slack: it is a cheap O(1) guard against an attacker-supplied
// megabyte "id" reaching the regexp engine at all, and it is the term the
// record-size derivation in retention.go counts (a bound nothing checks is a
// description, not a bound).
const MaxInviteIDLen = 64

// inviteIDRandBytes is the amount of crypto/rand entropy minted into an invite
// id: 10 bytes -> 16 base32 characters with no padding, plus the "inv-" prefix,
// for a 20-character id that always satisfies InviteIDPattern. This is the same
// shape and the same entropy as ids.GenerateBusID.
//
// 80 bits is not a security boundary here and must not be read as one: the
// invite id is a NAME, and the SECRET (secret.go) is the credential. What the
// entropy buys is that ids are not guessable in bulk and never collide.
const inviteIDRandBytes = 10

// GenerateInviteID mints a fresh opaque invite id using crypto/rand.
//
// A crypto/rand failure is a HARD ERROR. There is deliberately no fallback to a
// weaker source, exactly as in auth.newToken: an id minted from a predictable
// source lets an attacker anticipate which id the next invite will carry, and
// failing the mint loudly is strictly better than issuing one that looks fine.
func GenerateInviteID() (string, error) {
	buf := make([]byte, inviteIDRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("invite: reading %d bytes from crypto/rand for an invite id: %w; there is no weaker fallback", inviteIDRandBytes, err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	id := "inv-" + strings.ToLower(enc)
	if err := ValidateInviteID(id); err != nil {
		// Should never happen: the alphabet is a-z2-7 at a fixed length, which
		// always satisfies InviteIDPattern. Kept as a defensive re-validation
		// (ids.GenerateBusID does the same) because an id that fails its own
		// invariant must never reach a caller — it would be stored, then
		// rejected by the validation on the way back in, and the invite would
		// become unusable at the worst possible moment.
		return "", fmt.Errorf("invite: generated invite id %q failed validation: %w", id, err)
	}
	return id, nil
}

// ValidateInviteID checks the SHAPE of an untrusted invite id.
//
// The length check runs BEFORE the pattern match and is O(1) (Go strings carry
// their length), so an oversized id is rejected without ever being handed to
// the regexp engine — the discipline idem.ValidateKey and ids.ParseAgentID both
// use. An oversized id is NOT echoed back: it is attacker-chosen input about to
// be written to a log line, and an attacker must not get to choose a multiple
// of its own input back out of one.
func ValidateInviteID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: an invite id is required", ErrInvalidInviteID)
	}
	if len(id) > MaxInviteIDLen {
		return fmt.Errorf("%w: %d bytes, but an invite id is at most %d; it is not echoed here because it is oversized", ErrInvalidInviteID, len(id), MaxInviteIDLen)
	}
	if !inviteIDRegexp.MatchString(id) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidInviteID, id, InviteIDPattern)
	}
	return nil
}
