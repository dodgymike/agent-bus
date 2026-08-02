package client

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Everything in this file exists for one reason: THE BUS IS NOT TRUSTED TO
// PRODUCE SAFE TEXT.
//
// That is not paranoia about our own server, it is the threat model invariant
// 11 spells out. The invite blob is the trust anchor, so "whoever can
// substitute an invite can point an agent at a bus of their choosing" — and a
// bus of the attacker's choosing chooses the bytes in every field we print and
// every id we store.
//
// The damage is concrete. A response body of
//
//	{"error":"boom[2K\rbusctl: enrolled as bus1.admin\nall clear"}
//
// rendered verbatim to a terminal erases the line, overwrites it, and prints a
// fabricated success line. Terminal escapes can also set the window title and,
// where OSC 52 is enabled, write the clipboard. JSON output is safe —
// encoding/json escapes every byte below 0x20 — so this is specifically about
// what a HUMAN sees, and about what we WRITE TO THE STORE and re-print on
// every later command.

// maxDetailBytes bounds a server-supplied error detail. Long enough for a real
// message, short enough that a hostile bus cannot fill a scrollback.
const maxDetailBytes = 200

// safeText renders untrusted text for a terminal: every control character
// becomes a space, invalid UTF-8 is replaced, and the result is truncated on a
// RUNE boundary.
//
// Control characters are replaced rather than dropped so that a run of them
// cannot silently splice two words into one convincing token.
func safeText(s string, max int) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			b.WriteRune('�')
		case r < 0x20, r == 0x7f:
			// C0 controls and DEL: ESC, CR, LF, BEL, backspace, the lot.
			b.WriteByte(' ')
		case r >= 0x80 && r <= 0x9f:
			// C1 controls. A single 0x9b is CSI on some terminals, so these
			// are as dangerous as ESC-[ and are not merely "high bytes".
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if max <= 0 || len(out) <= max {
		return out
	}
	// Truncate on a rune boundary so the result is always valid UTF-8.
	cut := max
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return strings.TrimSpace(out[:cut]) + "…"
}

// serverIDPattern is the shape every id-like field the bus sends must have.
//
// Deliberately broader than the exact `<bus-id>.<name>-<n>` grammar: this is a
// SAFETY check, not a re-implementation of the server's id rules (invariant 1
// keeps the server authoritative on ids, and a client that re-derived the
// grammar would reject a legitimate future format). It admits exactly the
// characters internal/ids can produce and nothing that can move a cursor.
var serverIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,256}$`)

// validateServerField checks an id-like field the bus returned.
//
// Rejecting is right here, where sanitising would be wrong: these values are
// STORED and become the identity every later command prints and routes on.
// Quietly rewriting an id would leave the local store disagreeing with the bus
// about who we are, which is a worse failure than refusing the enrolment.
func validateServerField(op, field, value string) error {
	if value == "" {
		return newError(KindServer, op,
			"the bus returned an empty "+field,
			"check that --bus points at an agent-bus server")
	}
	if !serverIDPattern.MatchString(value) {
		return newError(KindServer, op,
			"the bus returned a "+field+" containing characters an agent id cannot contain: "+safeText(value, 60),
			"this is not a well-formed agent-bus response; check that --bus points at the bus you intended")
	}
	return nil
}

// validateServerTimestamp checks a timestamp field. It is stored and printed
// but never routed on, so the bar is "cannot move a cursor", not "is a valid
// RFC3339 instant" — a bus with a slightly different time format should not be
// unusable.
func validateServerTimestamp(op, field, value string) error {
	if value == "" {
		return nil
	}
	if safeText(value, 64) != value || len(value) > 64 {
		return newError(KindServer, op,
			"the bus returned a "+field+" containing control characters",
			"this is not a well-formed agent-bus response; check that --bus points at the bus you intended")
	}
	return nil
}
