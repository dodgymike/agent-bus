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
//	{"error":"boom[2K\ragent-busctl: enrolled as bus1.admin\nall clear"}
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

// TerminalSafe renders untrusted text for a terminal: every control character
// becomes a space, every bidi/zero-width codepoint becomes a space, and invalid
// UTF-8 becomes U+FFFD. It does NOT truncate — see safeText for that.
//
// # Why this is EXPORTED
//
// Invariant 7's third audience is an agent that EMBEDS this package, and an
// embedder rendering another agent's message body to a terminal needs exactly
// this function. Without it, the only options are to reach into this package's
// internals (impossible) or to write a second copy (which is what
// cmd/agent-busctl did, and what CLI-3-FU-SAFETEXT deleted). A security-relevant
// neutraliser that exists twice is one that decays silently: the two agreed on
// the day they were written and nothing would have failed when they stopped.
//
// # What it neutralises, and why REPLACE rather than DROP
//
//	C0 (< 0x20) and DEL   ESC, CR, LF, BEL, backspace, tab — the lot
//	C1 (0x80..0x9f)       a lone 0x9b is CSI on some terminals, so these are as
//	                      dangerous as ESC-[ and are not merely "high bytes"
//	bidi/zero-width       see IsBidiOrInvisible; none of these is a control, so
//	                      every ordinary control check misses all of them
//	invalid UTF-8         becomes U+FFFD
//
// Everything is REPLACED with a space rather than dropped, because dropping
// would splice the text either side into one convincing token: a dropped ESC
// turns "adm\x1bin" into "admin".
//
// keepNewlines is for a message BODY, where a newline is content and turning it
// into a space would mangle every multi-line message. It must NEVER be set for
// an id, a timestamp or an error detail: a line break in one of those is an
// attempt to forge a second line of output. Note that a caller keeping newlines
// takes on the job of making a continuation line unmistakable — cmd/agent-busctl
// indents them, so a multi-line body cannot read as several messages.
//
// This is about what a HUMAN sees. JSON output needs none of it: encoding/json
// escapes every byte below 0x20 already.
func TerminalSafe(s string, keepNewlines bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' && keepNewlines:
			b.WriteByte('\n')
		case r == utf8.RuneError:
			b.WriteRune('�')
		case r < 0x20, r == 0x7f:
			// C0 controls and DEL: ESC, CR, LF, BEL, backspace, the lot.
			b.WriteByte(' ')
		case r >= 0x80 && r <= 0x9f:
			// C1 controls. A single 0x9b is CSI on some terminals, so these
			// are as dangerous as ESC-[ and are not merely "high bytes".
			b.WriteByte(' ')
		case IsBidiOrInvisible(r):
			// The same forgery this function exists to stop, spelled in Unicode
			// rather than ANSI — and NOT caught by any control test, because none
			// of these is a control character. U+202E reverses the rest of the
			// line, so a bus that answers 500 with a bidi run can reorder the
			// text printed beside it. That matters more than it used to: the
			// failure line now also carries the idempotency-key remedy, where a
			// reordered "do NOT retry until the bus can durably accept again"
			// could read as an invitation to retry a poisoned write path.
			//
			// Replaced with a space rather than dropped, for the same reason the
			// controls are: dropping it would splice the text either side into
			// one convincing token.
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// safeText is TerminalSafe for a SERVER-SUPPLIED FIELD: newlines never survive,
// the result is trimmed, and it is bounded to max bytes on a RUNE boundary.
//
// The split from TerminalSafe is deliberate. The bound belongs to the caller,
// not to the renderer: a message body is legitimately long and must not be
// silently shortened, whereas an error detail from a hostile bus must not be
// able to fill a scrollback (maxDetailBytes). max <= 0 means no bound.
func safeText(s string, max int) string {
	out := strings.TrimSpace(TerminalSafe(s, false))
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

// IsBidiOrInvisible reports whether r changes how text is DISPLAYED without
// being visible itself.
//
// None of these is a C0, DEL or C1 control, so every ordinary control test
// misses all of them:
//
//	U+200B..U+200F  zero-width space/non-joiner/joiner, and the LTR/RTL marks
//	U+202A..U+202E  the legacy bidi embedding and override controls — U+202E
//	                (RIGHT-TO-LEFT OVERRIDE) visually REVERSES the rest of the
//	                line, which is how bus-chosen text can be made to read as
//	                something else entirely
//	U+2066..U+2069  the isolate forms of the same thing
//	U+FEFF          zero-width no-break space (BOM) — invisible mid-string
//
// Neither server-supplied text nor a message body has a legitimate use for any
// of them. Real bidirectional text (Arabic, Hebrew) renders correctly from its
// own character properties; these codepoints exist to OVERRIDE that.
//
// It is exported alongside TerminalSafe because the two answer different
// questions and both have callers. TerminalSafe REWRITES, which is right when
// something must be printed; this predicate lets a caller DISQUALIFY instead,
// which is right when the value is about to be handed to a consumer that will
// print it later — cmd/agent-busctl uses it that way for the `text` field of its
// NDJSON stream, where `jq -r .text` strips the JSON escaping and pipes the
// result straight at a terminal.
func IsBidiOrInvisible(r rune) bool {
	switch {
	case r >= 0x200b && r <= 0x200f:
		return true
	case r >= 0x202a && r <= 0x202e:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0xfeff:
		return true
	}
	return false
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

// serverTimestampPattern is the character set an RFC3339-ish instant needs, and
// nothing else.
//
// Digits, the letters (T, Z, and any alphabetic zone abbreviation a differently
// formatted bus might use), ':', '+', '-', '.', and a space so "2026-08-02
// 10:00:00Z" is still accepted. The bound is 64 bytes, which is roughly twice
// RFC3339Nano's length.
//
// Why this is a WHITELIST and not merely a control-character check: a timestamp
// was the one printed field with no safe alphabet. Rejecting controls and
// capping the length still admitted every other codepoint in Unicode, including
// U+202E RIGHT-TO-LEFT OVERRIDE — and this value is printed, unpadded, at the
// START of a line by `agent-busctl watch` (humanTime) and inside a column by
// `agent-busctl agents` (shortTimestamp). One override character there visually
// reorders the rest of the line, so the audit trail can be made to read as
// though a message came from a different agent. There is no legitimate instant
// that needs a character outside this set.
var serverTimestampPattern = regexp.MustCompile(`^[0-9A-Za-z:+.\- ]{1,64}$`)

// validateServerTimestamp checks a timestamp field. It is stored and printed
// but never routed on, so the bar is "cannot be made to display as something
// else", not "is a valid RFC3339 instant" — a bus with a slightly different
// time format should not be unusable. An empty value is fine: the bus is not
// required to send one.
func validateServerTimestamp(op, field, value string) error {
	if value == "" {
		return nil
	}
	if !serverTimestampPattern.MatchString(value) {
		return newError(KindServer, op,
			"the bus returned a "+field+" that is not a plain timestamp",
			"this is not a well-formed agent-bus response; check that --bus points at the bus you intended")
	}
	return nil
}

// contentHashPattern is a hex SHA-256 and nothing else: 64 lowercase hex digits.
//
// It was previously checked with validateServerField, which admits up to 256
// characters of [A-Za-z0-9._-] — a shape test so loose that "not-a-hash" passes
// it. The value is printed by `agent-busctl send` and carried in every --json record,
// and a caller comparing it against its own digest is entitled to assume it is
// at least the right KIND of thing.
//
// This checks the SHAPE only. The VALUE is compared against the decoded body on
// the read path by verifyMessageBody (messages.go), which runs after this and
// relies on it: a shape check first means a mismatch is reported as "these two
// hashes differ" rather than "this is not a hash at all".
var contentHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validateServerContentHash checks a content_sha256 field. An empty value is
// accepted — the bus does not always send one — but a present value must be a
// hex SHA-256.
func validateServerContentHash(op, field, value string) error {
	if value == "" {
		return nil
	}
	if !contentHashPattern.MatchString(value) {
		return newError(KindServer, op,
			"the bus returned a "+field+" that is not a hex SHA-256: "+safeText(value, 80),
			"this is not a well-formed agent-bus response; check that --bus points at the bus you intended")
	}
	return nil
}
