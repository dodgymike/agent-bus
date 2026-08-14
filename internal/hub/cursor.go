package hub

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// unknownCursorVersionLog and unknownCursorVersionOnce carry the ONE line
// DecodeCursor emits. It is a package-level logger rather than a Hub field
// because DecodeCursor is a package FUNCTION — internal/httpapi calls it
// without a hub — and threading a logger through would change a signature this
// task has no business changing.
//
// os.Stderr, not io.Discard, for the reason store.Options.Logger gives: a
// discarding default makes the real bus silent about the one thing an operator
// needs to see. WARN is the only level emitted here.
var (
	unknownCursorVersionLog  = logging.New(os.Stderr, logging.LevelWarn)
	unknownCursorVersionOnce sync.Once
)

// clipCursorVersion bounds the attacker-chosen version fragment before it
// reaches a log line. MaxCursorLen already bounds the whole blob, but the
// version field is echoed and a short one is all any real version needs.
func clipCursorVersion(v string) string {
	const max = 16
	if len(v) <= max {
		return v
	}
	return v[:max] + "..."
}

// cursorVersion prefixes every cursor this build issues. It is inside the
// opaque blob, not on the wire beside it, so a client cannot be tempted to
// parse or construct one.
//
// The number is RESERVED, never chosen by eyeballing this constant: value 1
// covered the shipped v1 cursor (a SEQUENCE) and value 2 was allocated from the
// Spec Server `cursor-format-version` namespace for this one (a DELIVERY
// POSITION, store.Message.Pos — SIGN-1-FU-REORDER-WATERMARK).
//
// v2 exists because the two counters are not interchangeable: a v1 cursor's
// number is a sequence, and reading it as a position would land a returning
// client at an arbitrary point in the stream. DecodeCursor therefore does not
// try to translate one into the other — there is no mapping — it REMAPS an
// unrecognised version to position 0. See there for why that is not an error.
const cursorVersion = "v2"

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

// DecodeCursor turns a client-supplied cursor back into a DELIVERY POSITION,
// checking it belongs to agentID.
//
// An EMPTY cursor is valid and means position 0: "I have seen nothing". A new
// agent therefore reads back through the whole retention window, which is the
// honest answer to "what have I not seen" and is why the caller paginates.
//
// # AN UNKNOWN VERSION IS REMAPPED TO 0, NOT REJECTED — this is load-bearing
//
// A cursor issued by another build carries a number in a counter this build
// does not use (a v1 cursor holds a SEQUENCE; this build's cursors hold a
// POSITION). There is no translation, so the choice is between rejecting it and
// replaying the retention window once.
//
// It must not be a rejection, and the reason is a WEDGE rather than a
// preference: ErrInvalidCursor surfaces as a 400, the client renders that as
// KindRejected, client.watchShouldRetry returns false, and NOTHING ever clears
// the stored cursor — so the same poisoned value is presented again on every
// subsequent watch, for ever. A build upgrade would permanently silence every
// agent that had ever stored a cursor. Remapping costs one replay of the
// retention window, which at-least-once delivery already permits (invariant 10:
// duplicates are the normal steady state, and the recipient's idempotency is
// what handles them).
//
// THE REMAP HAPPENS AFTER THE AGENT-BINDING CHECK, deliberately. Returning
// early on the version branch would let a cursor issued to a DIFFERENT agent be
// accepted as "unknown version, start at 0" — silently discarding the one check
// that turns "agent A presented agent B's cursor" from a no-op into a visible
// rejection. Order the two the other way and that check becomes bypassable by
// changing one byte of the blob.
//
// Every OTHER failure — oversized, not base64url, wrong field count, malformed
// position — still returns ErrInvalidCursor. The cursor VALUE is never echoed
// into the error: it is untrusted, attacker-chosen input on its way to a log
// line.
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
	// THE BINDING CHECK RUNS FIRST — before the version branch below. See the
	// doc comment: an early return on an unknown version would let another
	// agent's cursor through unchecked.
	if parts[1] != agentID {
		// The BOUND id is not echoed either: it names another agent, and a
		// probing client must not learn from the error whether the id it
		// stuffed in exists.
		return 0, fmt.Errorf("%w: this cursor was issued to a different agent", ErrInvalidCursor)
	}
	if parts[0] != cursorVersion {
		// ACCEPTED AND REMAPPED to "I have seen nothing", not rejected.
		//
		// Logged ONCE for the lifetime of the process, and that bound is not
		// tidiness: the version field is attacker-chosen bytes off a request, so
		// an uncapped line here is a log-flood any authenticated client could
		// drive by varying one byte per poll. One line names the first
		// unrecognised version seen, which is what an operator needs to know a
		// rollback or a stale client is in play; the remap itself is not an
		// error condition worth repeating.
		unknownCursorVersionOnce.Do(func() {
			unknownCursorVersionLog.Warn("a cursor carrying an UNRECOGNISED version was accepted and remapped to position 0, so this agent replays the retention window once. This is expected after an upgrade or a rollback (cursors moved from a sequence to a delivery position); rejecting it would wedge the client for ever, because a rejected cursor is never cleared and would be re-presented on every poll. Further occurrences are NOT logged",
				"version_seen", clipCursorVersion(parts[0]),
				"version_issued", cursorVersion,
			)
		})
		return 0, nil
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
