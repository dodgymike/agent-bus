package ids

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// TestMessageIDRoundTrip covers MessageID -> ParseMessageID round-tripping
// over a table including a minted GenerateBusID() id, single-char bus ids,
// seq 1, and seq math.MaxUint64.
func TestMessageIDRoundTrip(t *testing.T) {
	mintedBusID, err := GenerateBusID()
	if err != nil {
		t.Fatalf("GenerateBusID(): %v", err)
	}

	tests := []struct {
		name  string
		busID string
		seq   uint64
	}{
		{"minted bus id, small seq", mintedBusID, 42},
		{"single-char bus id", "a", 1},
		{"single-char bus id, seq 1", "b", 1},
		{"seq max uint64", "bus-x", math.MaxUint64},
		{"bus id containing dash", "bus-with-dashes", 7},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			id, err := MessageID(tt.busID, tt.seq)
			if err != nil {
				t.Fatalf("MessageID(%q, %d): unexpected error %v", tt.busID, tt.seq, err)
			}

			gotBusID, gotSeq, err := ParseMessageID(id)
			if err != nil {
				t.Fatalf("ParseMessageID(%q): unexpected error %v", id, err)
			}
			if gotBusID != tt.busID {
				t.Fatalf("ParseMessageID(%q) busID = %q, want %q", id, gotBusID, tt.busID)
			}
			if gotSeq != tt.seq {
				t.Fatalf("ParseMessageID(%q) seq = %d, want %d", id, gotSeq, tt.seq)
			}
		})
	}
}

// TestMessageIDRejectsInvalidInput covers MessageID's own validation: seq 0
// is always rejected, and an invalid bus id is rejected too.
func TestMessageIDRejectsInvalidInput(t *testing.T) {
	t.Run("seq zero", func(t *testing.T) {
		if _, err := MessageID("bus-x", 0); err == nil {
			t.Fatalf("MessageID(%q, 0) = nil error, want an error (seq 0 means unset)", "bus-x")
		}
	})

	t.Run("invalid bus id", func(t *testing.T) {
		if _, err := MessageID("bus.with.dots", 1); err == nil {
			t.Fatalf("MessageID(%q, 1) = nil error, want an error ('.' is forbidden in a bus id)", "bus.with.dots")
		}
	})

	t.Run("empty bus id", func(t *testing.T) {
		if _, err := MessageID("", 1); err == nil {
			t.Fatalf("MessageID(\"\", 1) = nil error, want an error")
		}
	})
}

// TestParseMessageIDRejectsMalformed is table-driven over every malformed
// spelling ParseMessageID must reject. Each case asserts only that an error
// is returned, never a specific error string.
func TestParseMessageIDRejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"no separator at all", "busx7"},
		{"empty bus half", "-7"},
		{"empty seq half", "bus-x-"},
		{"leading zero", "bus-x-007"},
		{"leading plus", "bus-x-+7"},
		{"non-digit letter suffix", "bus-x-7a"},
		{"non-digit embedded space", "bus-x- 7"},
		{"non-digit underscore", "bus-x-7_0"},
		{"seq zero", "bus-x-0"},
		{"seq all zeros", "bus-x-000"},
		{"seq overflow past 64 bits", "bus-x-18446744073709551616"},
		{"bus half containing dot", "bus.x-7"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			busID, seq, err := ParseMessageID(tt.id)
			if err == nil {
				t.Fatalf("ParseMessageID(%q) = (%q, %d, nil), want an error", tt.id, busID, seq)
			}
		})
	}
}

// TestParseMessageIDRejectsOversizedInputWithoutEchoingIt pins the length cap.
// The longest well-formed id is MaxMessageIDLen bytes, so anything longer is
// rejected before any formatting happens -- and the rejection must NOT quote
// the input, because %q escapes a control byte to four characters and would
// hand an attacker a multiple of their own input back in a log line.
func TestParseMessageIDRejectsOversizedInputWithoutEchoingIt(t *testing.T) {
	longest := strings.Repeat("b", 64) + "-" + "18446744073709551615"
	if len(longest) != MaxMessageIDLen {
		t.Fatalf("test setup: longest valid id is %d bytes, want MaxMessageIDLen = %d", len(longest), MaxMessageIDLen)
	}
	if _, _, err := ParseMessageID(longest); err != nil {
		t.Fatalf("ParseMessageID(<%d-byte maximal id>) = %v, want nil: the cap must not reject the longest VALID id", len(longest), err)
	}

	oversized := strings.Repeat("\x00", 1<<20) + "-7"
	_, _, err := ParseMessageID(oversized)
	if err == nil {
		t.Fatalf("ParseMessageID(<%d-byte id>) = nil error, want rejection past MaxMessageIDLen = %d", len(oversized), MaxMessageIDLen)
	}
	if got := len(err.Error()); got > 200 {
		t.Fatalf("error string is %d bytes for a %d-byte input; it must not echo the input", got, len(oversized))
	}
	if strings.Contains(err.Error(), "\x00") || strings.Contains(err.Error(), `\x00`) {
		t.Fatalf("error string echoes the attacker-controlled input: %q", err.Error())
	}
}

// TestParseMessageIDLeadingMinusIsStructurallyUnreachable documents a
// contract subtlety: because ParseMessageID splits on the LAST '-', any '-'
// that would appear in the sequence half is, by construction, never in the
// sequence half at all — it is always consumed as part of (or as) the
// separator, extending the bus id instead. There is therefore no message id
// string that produces a sequence half beginning with '-': what looks like a
// "negative sequence" attempt collapses into the documented-correct
// trailing-dash bus id case instead (see
// TestParseMessageIDTrailingDashBusIDIsLegal). This test pins that fact down
// explicitly rather than asserting a rejection that cannot be reached.
func TestParseMessageIDLeadingMinusIsStructurallyUnreachable(t *testing.T) {
	for _, id := range []string{"bus-x--7", "bus---7", "bus-x---7"} {
		_, seqPart, ok := lastMinusSplit(id)
		if !ok {
			t.Fatalf("test setup: %q has no '-' separator", id)
		}
		if strings.Contains(seqPart, "-") {
			t.Fatalf("sequence half of %q = %q, contains '-'; this is supposed to be structurally impossible via LastIndex splitting", id, seqPart)
		}
		if _, _, err := ParseMessageID(id); err != nil {
			t.Fatalf("ParseMessageID(%q) = %v, want nil error: a trailing '-' on the bus id is legal, so this cannot be a rejected \"leading minus\" case", id, err)
		}
	}
}

// lastMinusSplit mirrors ParseMessageID's own splitting rule, for the
// purposes of the documentation test above only.
func lastMinusSplit(id string) (bus, seq string, ok bool) {
	i := strings.LastIndex(id, messageIDSep)
	if i < 0 {
		return "", "", false
	}
	return id[:i], id[i+len(messageIDSep):], true
}

// TestParseMessageIDTrailingDashBusIDIsLegal is the case the implementer
// flagged explicitly: "bus-x--7" is documented-correct, NOT a bug.
// ParseMessageID("bus-x--7") must return busID "bus-x-" and seq 7, because a
// trailing '-' is legal under BusIDPattern and MessageID("bus-x-", 7)
// produces exactly that string. Assert the round-trip in both directions.
func TestParseMessageIDTrailingDashBusIDIsLegal(t *testing.T) {
	const wantBusID = "bus-x-"
	const wantSeq = uint64(7)

	if err := ValidateBusID(wantBusID); err != nil {
		t.Fatalf("ValidateBusID(%q): %v, want nil (a trailing '-' is legal under BusIDPattern)", wantBusID, err)
	}

	// Direction 1: MessageID(busID, seq) -> the expected string.
	built, err := MessageID(wantBusID, wantSeq)
	if err != nil {
		t.Fatalf("MessageID(%q, %d): unexpected error %v", wantBusID, wantSeq, err)
	}
	if built != "bus-x--7" {
		t.Fatalf("MessageID(%q, %d) = %q, want %q", wantBusID, wantSeq, built, "bus-x--7")
	}

	// Direction 2: parsing that exact string recovers the same pair.
	gotBusID, gotSeq, err := ParseMessageID("bus-x--7")
	if err != nil {
		t.Fatalf("ParseMessageID(%q): unexpected error %v", "bus-x--7", err)
	}
	if gotBusID != wantBusID {
		t.Fatalf("ParseMessageID(%q) busID = %q, want %q", "bus-x--7", gotBusID, wantBusID)
	}
	if gotSeq != wantSeq {
		t.Fatalf("ParseMessageID(%q) seq = %d, want %d", "bus-x--7", gotSeq, wantSeq)
	}
}

// TestParseMessageIDErrorIsNotSentinelWrapped is a small sanity check that
// ParseMessageID's malformed-input errors are ordinary errors (not, say,
// ErrSequenceExhausted or ErrFloorBelowIssued), so callers using errors.Is
// against the Sequence sentinels never mistake a parse failure for one of
// those allocator conditions.
func TestParseMessageIDErrorIsNotSentinelWrapped(t *testing.T) {
	_, _, err := ParseMessageID("bus-x-0")
	if err == nil {
		t.Fatalf("ParseMessageID(%q) = nil error, want an error", "bus-x-0")
	}
	if errors.Is(err, ErrSequenceExhausted) || errors.Is(err, ErrFloorBelowIssued) {
		t.Fatalf("ParseMessageID(%q) error unexpectedly matches a Sequence sentinel: %v", "bus-x-0", err)
	}
}
