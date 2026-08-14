package hub_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/hub"
)

// ---------------------------------------------------------------------------
// MSG-4 — history, pagination and the opaque cursor
//
// The load-bearing claim in this file is the LAST one: a cursor carries a
// POSITION and nothing else, so forging one can never widen what its holder is
// allowed to see. Everything above it is the pagination contract that makes the
// cursor usable in the first place.
// ---------------------------------------------------------------------------

// issuedCursorVersion reads the version field out of a cursor this build
// actually issues.
//
// The constant is unexported, so a test that wants to corrupt some OTHER field
// of a cursor has to spell the version out — and a spelled-out version that
// falls behind a bump stops testing what it names and starts testing the
// version branch, silently. Deriving it keeps the forgery fixtures pointed at
// the field they mean to corrupt.
func issuedCursorVersion(t *testing.T, agentID string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(hub.EncodeCursor(agentID, 1))
	if err != nil {
		t.Fatalf("a cursor this build issued is not base64url: %v", err)
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		t.Fatalf("a cursor this build issued has %d fields, want 3: %q", len(parts), raw)
	}
	if parts[0] == "" {
		t.Fatal("a cursor this build issued carries an EMPTY version field")
	}
	return parts[0]
}

func TestMessageHistoryCursor(t *testing.T) {
	t.Run("EmptyCursorIsPositionZeroAndYieldsEverything", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		const n = 5
		var want []string
		for i := 0; i < n; i++ {
			res, err := mintedBroadcast(t, h, hub.BroadcastRequest{
				Sender:         a,
				Body:           []byte(fmt.Sprintf("m%d", i)),
				IdempotencyKey: fmt.Sprintf("k-empty-%d", i),
			})
			if err != nil {
				t.Fatalf("Broadcast %d: %v", i, err)
			}
			want = append(want, res.MessageID)
		}

		pos, err := hub.DecodeCursor(b, "")
		if err != nil {
			t.Fatalf("DecodeCursor(%q, \"\"): %v", b, err)
		}
		if pos != 0 {
			t.Fatalf("an empty cursor decoded to %d, want 0", pos)
		}

		batch := mustHistory(t, h, b, pos, hub.MaxBatchLimit)
		if len(batch.Messages) != n {
			t.Fatalf("history from an empty cursor returned %d messages, want %d", len(batch.Messages), n)
		}
		for i, m := range batch.Messages {
			if m.ID != want[i] {
				t.Fatalf("history[%d] = %s, want %s", i, m.ID, want[i])
			}
		}
	})

	t.Run("PaginationNeverSkipsAndNeverRepeats", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		const n = 5
		var published []string
		for i := 0; i < n; i++ {
			res, err := mintedBroadcast(t, h, hub.BroadcastRequest{
				Sender:         a,
				Body:           []byte(fmt.Sprintf("page-%d", i)),
				IdempotencyKey: fmt.Sprintf("k-page-%d", i),
			})
			if err != nil {
				t.Fatalf("Broadcast %d: %v", i, err)
			}
			published = append(published, res.MessageID)
		}

		wantSizes := []int{2, 2, 1}
		wantMore := []bool{true, true, false}

		cursor := ""
		var got []string
		for round, size := range wantSizes {
			pos, err := hub.DecodeCursor(b, cursor)
			if err != nil {
				t.Fatalf("round %d: DecodeCursor: %v", round, err)
			}
			batch := mustHistory(t, h, b, pos, 2)
			if len(batch.Messages) != size {
				t.Fatalf("round %d returned %d messages, want %d", round, len(batch.Messages), size)
			}
			if batch.More != wantMore[round] {
				t.Fatalf("round %d: More = %v, want %v (batch of %d, limit 2)", round, batch.More, wantMore[round], len(batch.Messages))
			}
			for _, m := range batch.Messages {
				got = append(got, m.ID)
			}
			cursor = hub.EncodeCursor(b, batch.Cursor)
		}
		if len(got) != n {
			t.Fatalf("pagination yielded %d messages over %d rounds, want %d", len(got), len(wantSizes), n)
		}
		seen := map[string]int{}
		for i, id := range got {
			if id != published[i] {
				t.Fatalf("paginated[%d] = %s, want %s (full: %v)", i, id, published[i], got)
			}
			seen[id]++
		}
		for id, count := range seen {
			if count != 1 {
				t.Fatalf("message %s was delivered %d times across the pages", id, count)
			}
		}
	})

	t.Run("EmptyBatchLeavesTheCursorByteIdentical", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		if _, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("only one"), IdempotencyKey: "k-tail"}); err != nil {
			t.Fatalf("Broadcast: %v", err)
		}
		// The caught-up cursor comes from a real read, NOT from the message's
		// sequence: a cursor is a DELIVERY POSITION and the two counters are
		// unrelated (SIGN-1-FU-REORDER-WATERMARK).
		caughtUp := hub.EncodeCursor(b, mustHistory(t, h, b, 0, 10).Cursor)

		pos, err := hub.DecodeCursor(b, caughtUp)
		if err != nil {
			t.Fatalf("DecodeCursor: %v", err)
		}
		batch := mustHistory(t, h, b, pos, 10)
		if len(batch.Messages) != 0 {
			t.Fatalf("a caught-up reader got %d messages, want 0", len(batch.Messages))
		}
		if batch.More {
			t.Fatal("an empty batch reported More")
		}
		if batch.Cursor != pos {
			t.Fatalf("an empty batch moved the cursor from %d to %d", pos, batch.Cursor)
		}
		if again := hub.EncodeCursor(b, batch.Cursor); again != caughtUp {
			t.Fatalf("re-encoding the cursor of an empty batch gave %q, want the byte-identical %q", again, caughtUp)
		}
	})

	t.Run("RoundTrip", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha")
		a := agentID(t, h.BusID(), "alpha")

		positions := []uint64{0, 1, 42, math.MaxUint64}
		if len(positions) == 0 {
			t.Fatal("the round-trip table is empty")
		}
		checked := 0
		for _, n := range positions {
			enc := hub.EncodeCursor(a, n)
			if len(enc) > hub.MaxCursorLen {
				t.Fatalf("EncodeCursor(%q, %d) is %d bytes, over MaxCursorLen %d", a, n, len(enc), hub.MaxCursorLen)
			}
			got, err := hub.DecodeCursor(a, enc)
			if err != nil {
				t.Fatalf("DecodeCursor(%q, EncodeCursor(%q, %d)): %v", a, a, n, err)
			}
			if got != n {
				t.Fatalf("round trip of %d gave %d", n, got)
			}
			checked++
		}
		if checked != len(positions) {
			t.Fatalf("round-tripped %d positions, want %d", checked, len(positions))
		}
	})

	t.Run("Forgery", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		// The malformed-POSITION fixtures are built with the version this build
		// actually issues, read back out of a real cursor rather than written as a
		// literal. A literal that fell behind a version bump would silently stop
		// testing the position field and start testing the version branch — which
		// is exactly what happened when cursors moved to v2
		// (SIGN-1-FU-REORDER-WATERMARK): every one of these cases short-circuited
		// on the version and proved nothing about position parsing.
		ver := issuedCursorVersion(t, b)

		cases := []struct {
			name   string
			cursor string
		}{
			{"IssuedToAnotherAgent", hub.EncodeCursor(a, 5)},
			// THE BINDING CHECK RUNS BEFORE THE VERSION REMAP. An OLD-version
			// cursor issued to another agent must still be refused — an
			// implementation that returned early on the unknown version would
			// accept this one as "start from zero" and skip the one check that
			// makes a cursor agent-bound.
			{"OldVersionIssuedToAnotherAgent", base64.RawURLEncoding.EncodeToString([]byte("v1|" + a + "|5"))},
			{"NotBase64URL", "!!!not base64!!!"},
			{"StandardBase64Padding", base64.StdEncoding.EncodeToString([]byte(ver + "|" + b + "|5"))},
			{"WrongFieldCountTooFew", base64.RawURLEncoding.EncodeToString([]byte(ver + "|" + b))},
			{"WrongFieldCountTooMany", base64.RawURLEncoding.EncodeToString([]byte(ver + "|" + b + "|5|extra"))},
			{"LeadingZeroPosition", base64.RawURLEncoding.EncodeToString([]byte(ver + "|" + b + "|007"))},
			{"SignedPosition", base64.RawURLEncoding.EncodeToString([]byte(ver + "|" + b + "|+7"))},
			{"NegativePosition", base64.RawURLEncoding.EncodeToString([]byte(ver + "|" + b + "|-7"))},
			{"EmptyPosition", base64.RawURLEncoding.EncodeToString([]byte(ver + "|" + b + "|"))},
			{"NonNumericPosition", base64.RawURLEncoding.EncodeToString([]byte(ver + "|" + b + "|seven"))},
			{"PositionOverflowsUint64", base64.RawURLEncoding.EncodeToString([]byte(ver + "|" + b + "|18446744073709551616"))},
			{"Oversized", strings.Repeat("A", hub.MaxCursorLen+1)},
		}
		if len(cases) == 0 {
			t.Fatal("the forgery table is empty")
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				pos, err := hub.DecodeCursor(b, c.cursor)
				if !errors.Is(err, hub.ErrInvalidCursor) {
					t.Fatalf("DecodeCursor(%q, <%s>) = (%d, %v), want ErrInvalidCursor", b, c.name, pos, err)
				}
				if pos != 0 {
					t.Fatalf("a rejected cursor still returned position %d; a caller that ignores the error must not get a usable position", pos)
				}
				// The untrusted value must never be echoed back into an error
				// that lands in a log line.
				if len(c.cursor) > 8 && strings.Contains(err.Error(), c.cursor) {
					t.Fatalf("the error echoes the attacker-supplied cursor: %v", err)
				}
			})
		}
		// A cursor this agent really was issued still decodes, so the rejections
		// above are discrimination rather than a blanket refusal.
		if _, err := hub.DecodeCursor(b, hub.EncodeCursor(b, 5)); err != nil {
			t.Fatalf("a legitimate cursor for %s was rejected: %v", b, err)
		}
	})

	// REVISED BY SIGN-1-FU-REORDER-WATERMARK. "UnknownVersion" and
	// "EmptyVersion" used to sit in the forgery table above and expect
	// ErrInvalidCursor. They now mean ACCEPTED AND REMAPPED, and the reason is a
	// WEDGE rather than a preference.
	//
	// Cursors moved from a SEQUENCE (v1) to a DELIVERY POSITION (v2), and there
	// is no translation between the two counters. Rejecting the old value would
	// answer 400, which the client renders as KindRejected; watchShouldRetry
	// returns false for that, and NOTHING ever clears the stored cursor — so the
	// same poisoned value is re-presented on every poll, for ever. One build
	// upgrade would permanently silence every agent that had ever stored a
	// cursor. Remapping costs one replay of the retention window, which
	// at-least-once delivery already permits (invariant 10).
	t.Run("AnUnknownVersionIsRemappedToPositionZeroNotRejected", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		b := agentID(t, h.BusID(), "beta")

		cases := []struct {
			name    string
			version string
		}{
			// The real one: a cursor this bus itself issued before the split.
			{"TheShippedV1Cursor", "v1"},
			{"EmptyVersion", ""},
			{"AFutureVersion", "v99"},
			{"NotAVersionAtAll", "garbage"},
		}
		if len(cases) == 0 {
			t.Fatal("the unknown-version table is empty")
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				cursor := base64.RawURLEncoding.EncodeToString([]byte(c.version + "|" + b + "|5"))
				pos, err := hub.DecodeCursor(b, cursor)
				if err != nil {
					t.Fatalf("DecodeCursor with version %q returned %v, want nil — a rejected cursor is never "+
						"cleared by the client and would be re-presented for ever, wedging that agent "+
						"permanently (SIGN-1-FU-REORDER-WATERMARK)", c.version, err)
				}
				if pos != 0 {
					t.Fatalf("DecodeCursor with version %q returned position %d, want 0 — the number inside is a "+
						"value in a counter this build does not use, so carrying it forward would drop the "+
						"reader at an arbitrary point in the stream", c.version, pos)
				}
			})
		}

		// AND THE REMAP DOES NOT SWALLOW THE BINDING CHECK. The same unknown
		// version, bound to somebody else, is still refused — this is the ordering
		// requirement, asserted here as well as in the forgery table so that
		// whichever of the two a future edit reads, it sees it.
		other := agentID(t, h.BusID(), "alpha")
		wrong := base64.RawURLEncoding.EncodeToString([]byte("v1|" + other + "|5"))
		if pos, err := hub.DecodeCursor(b, wrong); !errors.Is(err, hub.ErrInvalidCursor) {
			t.Fatalf("a v1 cursor issued to %s was accepted for %s: (%d, %v). The version remap MUST run after "+
				"the agent-binding check, or changing one byte of the version field bypasses the binding",
				other, b, pos, err)
		}
	})

	t.Run("AForgedCursorCannotWidenVisibility", func(t *testing.T) {
		// THE POINT OF THE WHOLE FILE. The filter is the authenticated
		// principal, never the cursor: rewinding to position 0 replays only what
		// the holder was always entitled to see.
		h, _, _ := newTestHub(t, "alpha", "beta", "gamma")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")
		g := agentID(t, h.BusID(), "gamma")

		dm, err := mintedSend(t, h, hub.SendRequest{Sender: g, To: a, Body: []byte("for alpha's eyes"), IdempotencyKey: "k-secret"})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		bc, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: g, Body: []byte("for everyone"), IdempotencyKey: "k-public"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}

		// Beta hand-builds a cursor for itself at position 0 — the widest read it
		// can ask for.
		rewound := hub.EncodeCursor(b, 0)
		pos, err := hub.DecodeCursor(b, rewound)
		if err != nil {
			t.Fatalf("DecodeCursor of a self-issued position-0 cursor: %v", err)
		}
		batch := mustHistory(t, h, b, pos, hub.MaxBatchLimit)

		if len(batch.Messages) == 0 {
			t.Fatal("beta's rewound history is empty, so this test proves nothing about filtering")
		}
		sawBroadcast := false
		for _, m := range batch.Messages {
			if m.ID == dm.MessageID {
				t.Fatalf("beta read DM %s, which is addressed to %s", dm.MessageID, a)
			}
			if m.ID == bc.MessageID {
				sawBroadcast = true
			}
		}
		if !sawBroadcast {
			t.Fatalf("beta cannot see broadcast %s, so the absence of the DM is not evidence of filtering", bc.MessageID)
		}

		// And presenting ALPHA's cursor does not help either: it is refused
		// outright, and even the position it carries buys nothing.
		if _, err := hub.DecodeCursor(b, hub.EncodeCursor(a, 0)); !errors.Is(err, hub.ErrInvalidCursor) {
			t.Fatalf("beta decoded a cursor issued to alpha: err = %v", err)
		}
	})

	t.Run("LimitIsClampedOnHistory", func(t *testing.T) {
		// Observable behaviour only: limit <= 0 must behave as
		// hub.DefaultBatchLimit, which is provable with DefaultBatchLimit+1
		// visible messages.
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		total := hub.DefaultBatchLimit + 1
		for i := 0; i < total; i++ {
			if _, err := mintedBroadcast(t, h, hub.BroadcastRequest{
				Sender:         a,
				Body:           []byte(fmt.Sprintf("clamp-%d", i)),
				IdempotencyKey: fmt.Sprintf("k-clamp-%d", i),
			}); err != nil {
				t.Fatalf("Broadcast %d: %v", i, err)
			}
		}

		for _, limit := range []int{0, -1, -1000} {
			batch := mustHistory(t, h, b, 0, limit)
			if len(batch.Messages) != hub.DefaultBatchLimit {
				t.Fatalf("History with limit %d returned %d messages, want DefaultBatchLimit (%d)", limit, len(batch.Messages), hub.DefaultBatchLimit)
			}
			if !batch.More {
				t.Fatalf("History with limit %d cut the batch short but did not report More", limit)
			}
		}
	})
}
