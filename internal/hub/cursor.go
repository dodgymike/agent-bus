package hub

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// cursorVersion prefixes every cursor this build issues. It is inside the
// opaque blob, not on the wire beside it, so a client cannot be tempted to
// parse or construct one.
const cursorVersion = "v1"

// cursorSep separates the three cursor fields. '|' is chosen because it cannot
// occur in either of the other two fields: an agent id is
// "<bus-id>.<name>-<n>" over [a-z0-9._-] (ids.AgentNamePattern and
// ids.BusIDPattern) and a sequence is decimal digits. A separator that could
// appear inside a field would make the encoding ambiguous, which is how a
// cursor for one agent gets read as a cursor for another.
const cursorSep = "|"

// MaxCursorLen bounds an inbound cursor string before it is decoded.
//
// A real cursor is the version, an agent id of at most ids.MaxAgentIDLen and 20
// digits of sequence, base64url-encoded: comfortably under 200 bytes. 512 is
// generous headroom and, more to the point, FINITE — an authenticated caller
// must not be able to push an unbounded string into the decoder on every poll.
const MaxCursorLen = 512

// EncodeCursor renders the opaque cursor for agentID at position after.
//
// # What a cursor is, and what it deliberately is NOT
//
// It is a POSITION and nothing else. It carries no entitlement: every batch is
// filtered with the AUTHENTICATED principal through store.Message.VisibleTo, so
// a client holding a cursor minted for another agent cannot read that agent's
// stream — the filter never consults the cursor.
//
// The agent id is embedded anyway, and DecodeCursor rejects a mismatch. That is
// defence in depth with a specific job: it turns "agent A presented agent B's
// cursor" from a silent no-op into a rejected request an operator can see. A
// cursor moves between processes — it is written to a client's state file and
// handed back later — and a positional token that silently applies to whoever
// presents it is the kind of thing that becomes a bug the moment someone adds a
// second place that consults it.
//
// # Why it is not signed
//
// It does not need to be, and signing it would be a false promise. Forging a
// cursor for YOURSELF gains nothing: a lower position replays your own
// messages (at-least-once already permits duplicates) and a higher one skips
// them, which is self-inflicted. Forging one for ANOTHER agent gains nothing
// because the visibility filter uses the principal. A MAC here would protect a
// value whose integrity buys no security property, at the cost of a key to
// manage and rotate — see invariant 8, and invariant 9 on not reaching for
// crypto that is not solving a problem.
//
// The encoding is base64url so the cursor is safe in a query parameter, a
// header, a shell variable and a JSON string without escaping.
func EncodeCursor(agentID string, after uint64) string {
	raw := cursorVersion + cursorSep + agentID + cursorSep + strconv.FormatUint(after, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor turns a client-supplied cursor back into a position, checking it
// belongs to agentID.
//
// An EMPTY cursor is valid and means position 0: "I have seen nothing". A new
// agent therefore reads back through the whole retention window, which is the
// honest answer to "what have I not seen" and is why the caller paginates.
//
// Every failure returns ErrInvalidCursor. The cursor VALUE is never echoed into
// the error: it is untrusted, attacker-chosen input on its way to a log line.
func DecodeCursor(agentID, cursor string) (uint64, error) {
	if cursor == "" {
		return 0, nil
	}
	if len(cursor) > MaxCursorLen {
		return 0, fmt.Errorf("%w: %d bytes, the limit is %d; the value is not echoed because it is oversized", ErrInvalidCursor, len(cursor), MaxCursorLen)
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("%w: not base64url", ErrInvalidCursor)
	}
	parts := strings.Split(string(raw), cursorSep)
	if len(parts) != 3 {
		return 0, fmt.Errorf("%w: wrong field count", ErrInvalidCursor)
	}
	if parts[0] != cursorVersion {
		return 0, fmt.Errorf("%w: unknown cursor version", ErrInvalidCursor)
	}
	if parts[1] != agentID {
		// The BOUND id is not echoed either: it names another agent, and a
		// probing client must not learn from the error whether the id it
		// stuffed in exists.
		return 0, fmt.Errorf("%w: this cursor was issued to a different agent", ErrInvalidCursor)
	}
	// Rejects "+7", "-7", " 7" and, via the leading-zero check below, "007":
	// one position has exactly one spelling, for the same reason
	// ids.ParseMessageID insists on it — two spellings of one value defeat any
	// table keyed on the string form.
	if parts[2] == "" || (len(parts[2]) > 1 && parts[2][0] == '0') {
		return 0, fmt.Errorf("%w: malformed position", ErrInvalidCursor)
	}
	after, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: malformed position", ErrInvalidCursor)
	}
	return after, nil
}
