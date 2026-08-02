package ids

import (
	"math"
	"strings"
	"testing"
)

// TestAgentIDMintingRoundTrip covers AgentID -> ParseAgentID round-tripping
// over a table chosen to stress the two splitting rules ParseAgentID rests on:
// the FIRST '.' separates bus from agent, and the LAST '-' separates name from
// suffix. The interesting rows are the ones where a name itself contains,
// ends with, or ends with something that LOOKS like, the suffix separator.
func TestAgentIDMintingRoundTrip(t *testing.T) {
	mintedBusID, err := GenerateBusID()
	if err != nil {
		t.Fatalf("GenerateBusID(): %v", err)
	}

	tests := []struct {
		desc  string
		busID string
		name  string
		n     uint64
	}{
		{"minted bus id, small suffix", mintedBusID, "bob", 42},
		{"single-char bus id and name", "a", "b", 1},
		{"suffix 1", "bus-x", "bob", 1},
		{"suffix max uint64", "bus-x", "bob", math.MaxUint64},
		{"name containing dashes", "bus-x", "code-reviewer", 3},
		// "x-" yields "bus-x.x--1": the LAST '-' is the separator, so the
		// trailing dash stays in the name half where it belongs.
		{"name ending in a dash", "bus-x", "x-", 1},
		// "worker-2" yields "bus-x.worker-2-1", which a FIRST-dash split would
		// mis-read as name "worker" with suffix "2-1".
		{"name ending in -digits", "bus-x", "worker-2", 1},
		{"64-char name", "bus-x", strings.Repeat("a", 64), 9},
		{"64-char bus id", strings.Repeat("b", 64), "bob", 9},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.desc, func(t *testing.T) {
			id, err := AgentID(tt.busID, tt.name, tt.n)
			if err != nil {
				t.Fatalf("AgentID(%q, %q, %d): unexpected error %v", tt.busID, tt.name, tt.n, err)
			}

			gotBusID, gotName, gotN, err := ParseAgentID(id)
			if err != nil {
				t.Fatalf("ParseAgentID(%q): unexpected error %v", id, err)
			}
			if gotBusID != tt.busID {
				t.Fatalf("ParseAgentID(%q) busID = %q, want %q", id, gotBusID, tt.busID)
			}
			if gotName != tt.name {
				t.Fatalf("ParseAgentID(%q) name = %q, want %q", id, gotName, tt.name)
			}
			if gotN != tt.n {
				t.Fatalf("ParseAgentID(%q) suffix = %d, want %d", id, gotN, tt.n)
			}
		})
	}
}

// TestAgentIDMintingIDsAreUnambiguous is the bijection the last-dash split
// rests on. AgentNamePattern lets a name contain '-', so "<name>-<n>" is only
// an unambiguous encoding because the suffix is decimal digits only and can
// therefore never contain a '-': the last '-' in a well-formed id is always
// the one AgentID wrote. This test pins that claim as a property rather than
// as prose — distinct (name, suffix) pairs must produce distinct id strings,
// and each id must decode to exactly the pair it was built from. If that ever
// stopped holding, two different agents could share one routing and
// authorization subject (invariants 2 and 3).
func TestAgentIDMintingIDsAreUnambiguous(t *testing.T) {
	const busID = "bus-x"

	pairs := []struct {
		name string
		n    uint64
	}{
		{"worker-2", 1},   // -> bus-x.worker-2-1
		{"worker", 21},    // -> bus-x.worker-21
		{"worker-2-1", 1}, // -> bus-x.worker-2-1-1
		{"worker-2", 11},  // -> bus-x.worker-2-11
		{"worker", 211},   // -> bus-x.worker-211
		{"x-", 1},         // -> bus-x.x--1
		{"x", 1},          // -> bus-x.x-1
		{"x", 11},         // -> bus-x.x-11
		{"x-1", 1},        // -> bus-x.x-1-1
	}

	seen := make(map[string]int, len(pairs))
	for i, p := range pairs {
		id, err := AgentID(busID, p.name, p.n)
		if err != nil {
			t.Fatalf("AgentID(%q, %q, %d): unexpected error %v", busID, p.name, p.n, err)
		}
		if prev, dup := seen[id]; dup {
			t.Fatalf("AgentID(%q, %q, %d) = %q, the same id as (%q, %d); distinct (name, suffix) pairs must produce distinct ids",
				busID, p.name, p.n, id, pairs[prev].name, pairs[prev].n)
		}
		seen[id] = i

		gotBusID, gotName, gotN, err := ParseAgentID(id)
		if err != nil {
			t.Fatalf("ParseAgentID(%q): unexpected error %v", id, err)
		}
		if gotBusID != busID || gotName != p.name || gotN != p.n {
			t.Fatalf("ParseAgentID(%q) = (%q, %q, %d), want (%q, %q, %d)",
				id, gotBusID, gotName, gotN, busID, p.name, p.n)
		}
	}
	if len(seen) != len(pairs) {
		t.Fatalf("built %d distinct ids from %d distinct (name, suffix) pairs; the encoding is not injective", len(seen), len(pairs))
	}
}

// TestAgentIDMintingRejectsInvalidName is table-driven over ValidateAgentName
// and asserts AgentID agrees with it on every row: the validator is
// documented as the ONLY definition of "legal name" in this package, so a name
// AgentID accepted but ValidateAgentName rejected (or vice versa) would mean a
// second, undocumented definition exists.
//
// Each case asserts only that an error IS or IS NOT returned, never its text.
func TestAgentIDMintingRejectsInvalidName(t *testing.T) {
	tests := []struct {
		desc    string
		name    string
		wantErr bool
	}{
		{"empty", "", true},
		{"leading uppercase", "Bob", true},
		{"all uppercase", "BOB", true},
		{"embedded dot", "bo.b", true},
		{"leading dot", ".bob", true},
		{"leading dash", "-bob", true},
		{"leading underscore", "_bob", true},
		{"contains slash", "bo/b", true},
		{"contains space", "bo b", true},
		{"contains colon", "bo:b", true},
		{"contains at sign", "bo@b", true},
		{"contains NUL", "bo\x00b", true},
		{"contains newline", "bo\nb", true},
		{"non-ASCII latin", "böb", true},
		{"non-ASCII emoji", "b\U0001F600b", true},
		{"65 chars", strings.Repeat("a", 65), true},

		{"single letter", "a", false},
		{"single digit", "0", false},
		{"plain", "bob", false},
		{"dashed", "code-reviewer", false},
		{"underscore, dash and digit", "a_b-c9", false},
		{"exactly 64 chars", strings.Repeat("a", 64), false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.desc, func(t *testing.T) {
			err := ValidateAgentName(tt.name)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateAgentName(%q) = nil error, want an error", tt.name)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateAgentName(%q) = %v, want nil error", tt.name, err)
			}

			id, err := AgentID("bus-x", tt.name, 1)
			if tt.wantErr && err == nil {
				t.Fatalf("AgentID(%q, %q, 1) = (%q, nil), want an error to match ValidateAgentName", "bus-x", tt.name, id)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("AgentID(%q, %q, 1) = %v, want nil error to match ValidateAgentName", "bus-x", tt.name, err)
			}
		})
	}
}

// TestAgentIDMintingRejectsZeroSuffix pins the documented meaning of suffix 0:
// NameSuffixes never issues it (the first NextSuffix for a name returns 1), so
// a 0 reaching AgentID is an unset field, and formatting it would mint a
// real-looking id for an agent that holds no allocated suffix at all. The same
// value must therefore be unparseable, in every spelling.
func TestAgentIDMintingRejectsZeroSuffix(t *testing.T) {
	t.Run("AgentID rejects suffix 0", func(t *testing.T) {
		id, err := AgentID("bus-x", "bob", 0)
		if err == nil {
			t.Fatalf("AgentID(%q, %q, 0) = (%q, nil), want an error (suffix 0 means \"unset\")", "bus-x", "bob", id)
		}
	})

	for _, id := range []string{"bus-x.bob-0", "bus-x.bob-00", "bus-x.bob-000"} {
		id := id
		t.Run("ParseAgentID rejects "+id, func(t *testing.T) {
			busID, name, n, err := ParseAgentID(id)
			if err == nil {
				t.Fatalf("ParseAgentID(%q) = (%q, %q, %d, nil), want an error", id, busID, name, n)
			}
		})
	}
}

// TestAgentIDMintingParseRejectsMalformed is table-driven over every malformed
// spelling ParseAgentID must reject. Each case asserts only that an error is
// returned, never a specific error string.
func TestAgentIDMintingParseRejectsMalformed(t *testing.T) {
	tests := []struct {
		desc string
		id   string
	}{
		{"no dot separator at all", "bus-x-bob-1"},
		{"no dash in the agent half", "bus-x.bob"},
		{"empty bus id", ".bob-1"},
		{"empty name", "bus-x.-1"},
		{"uppercase name in an otherwise well-formed id", "bus-x.Bob-1"},
		// Two dots must fail as a bad NAME, not be silently re-split: the id is
		// cut at the FIRST '.', so the second one lands in the name half where
		// AgentNamePattern rejects it. "bus-x.a.b-1" is therefore never read as
		// bus "bus-x.a".
		{"two dots", "bus-x.a.b-1"},
		{"bus id containing a dot", "bu.s-x.bob-1"},
		{"leading zero suffix", "bus-x.bob-01"},
		{"leading zeros suffix", "bus-x.bob-007"},
		{"signed suffix", "bus-x.bob-+1"},
		{"whitespace in suffix", "bus-x.bob- 1"},
		{"trailing whitespace in suffix", "bus-x.bob-1 "},
		{"non-digit letter in suffix", "bus-x.bob-1x"},
		{"non-digit underscore in suffix", "bus-x.bob-1_0"},
		{"empty suffix", "bus-x.bob-"},
		{"suffix overflows uint64", "bus-x.bob-18446744073709551616"},
		{"empty string", ""},
		{"bare dot", "."},
		{"bare dash", "-"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.desc, func(t *testing.T) {
			busID, name, n, err := ParseAgentID(tt.id)
			if err == nil {
				t.Fatalf("ParseAgentID(%q) = (%q, %q, %d, nil), want an error", tt.id, busID, name, n)
			}
		})
	}
}

// TestAgentIDMintingTrailingDashNameIsLegal documents a contract subtlety that
// looks like a missing rejection and is not one.
//
// "bus-x.bob--1" is NOT a "negative suffix" that ParseAgentID fails to catch:
// because the agent half is split on the LAST '-', the extra dash is consumed
// as part of the NAME, giving name "bob-" and suffix 1 — and "bob-" is legal
// under AgentNamePattern (see the "name ending in a dash" row of
// TestAgentIDMintingRoundTrip, and TestParseMessageIDTrailingDashBusIDIsLegal
// for the identical situation one component further out). A suffix half
// beginning with '-' is structurally unreachable, so there is nothing to
// reject; this asserts the round-trip in both directions instead.
func TestAgentIDMintingTrailingDashNameIsLegal(t *testing.T) {
	const id = "bus-x.bob--1"
	const wantBusID = "bus-x"
	const wantName = "bob-"
	const wantN = uint64(1)

	if err := ValidateAgentName(wantName); err != nil {
		t.Fatalf("ValidateAgentName(%q) = %v, want nil (a trailing '-' is legal under AgentNamePattern)", wantName, err)
	}

	built, err := AgentID(wantBusID, wantName, wantN)
	if err != nil {
		t.Fatalf("AgentID(%q, %q, %d): unexpected error %v", wantBusID, wantName, wantN, err)
	}
	if built != id {
		t.Fatalf("AgentID(%q, %q, %d) = %q, want %q", wantBusID, wantName, wantN, built, id)
	}

	gotBusID, gotName, gotN, err := ParseAgentID(id)
	if err != nil {
		t.Fatalf("ParseAgentID(%q): unexpected error %v; a leading '-' on the suffix half is structurally unreachable, so this is the trailing-dash NAME case, not a signed suffix", id, err)
	}
	if gotBusID != wantBusID || gotName != wantName || gotN != wantN {
		t.Fatalf("ParseAgentID(%q) = (%q, %q, %d), want (%q, %q, %d)", id, gotBusID, gotName, gotN, wantBusID, wantName, wantN)
	}
}

// TestAgentIDMintingMaximalIDIsAccepted pins the length cap against the
// LONGEST id that can legitimately exist: a 64-byte bus id, a 64-byte name and
// math.MaxUint64. MaxAgentIDLen is derived from exactly those bounds, so this
// is the row where an off-by-one in the constant would start rejecting valid
// ids — a denial of enrolment to a legitimately-named agent.
func TestAgentIDMintingMaximalIDIsAccepted(t *testing.T) {
	busID := strings.Repeat("b", 64)
	name := strings.Repeat("a", 64)
	const n = uint64(math.MaxUint64)

	id, err := AgentID(busID, name, n)
	if err != nil {
		t.Fatalf("AgentID(<64-byte bus id>, <64-byte name>, math.MaxUint64): unexpected error %v", err)
	}
	if len(id) != MaxAgentIDLen {
		t.Fatalf("longest valid agent id is %d bytes, want MaxAgentIDLen = %d", len(id), MaxAgentIDLen)
	}

	gotBusID, gotName, gotN, err := ParseAgentID(id)
	if err != nil {
		t.Fatalf("ParseAgentID(<%d-byte maximal id>) = %v, want nil: the cap must not reject the longest VALID id", len(id), err)
	}
	if gotBusID != busID || gotName != name || gotN != n {
		t.Fatalf("ParseAgentID(<maximal id>) = (%q, %q, %d), want (<64 b's>, <64 a's>, %d)", gotBusID, gotName, gotN, n)
	}
}

// TestAgentIDMintingOversizedIDIsNotEchoed mirrors the message-id non-echo
// test. Past MaxAgentIDLen the content cannot matter — no such string is a
// valid id — so the rejection must NOT quote the input: %q escapes a control
// byte to four characters, so echoing would hand an attacker a multiple of
// their own chosen bytes back out in a log line.
func TestAgentIDMintingOversizedIDIsNotEchoed(t *testing.T) {
	oversized := strings.Repeat("\x00", 1<<20) + ".bob-1"
	if len(oversized) <= MaxAgentIDLen {
		t.Fatalf("test setup: input is %d bytes, which is not past MaxAgentIDLen = %d", len(oversized), MaxAgentIDLen)
	}

	_, _, _, err := ParseAgentID(oversized)
	if err == nil {
		t.Fatalf("ParseAgentID(<%d-byte id>) = nil error, want rejection past MaxAgentIDLen = %d", len(oversized), MaxAgentIDLen)
	}
	if got := len(err.Error()); got > 300 {
		t.Fatalf("error string is %d bytes for a %d-byte input; it must not echo the input", got, len(oversized))
	}
	if strings.Contains(err.Error(), "\x00") || strings.Contains(err.Error(), `\x00`) {
		t.Fatalf("error string echoes the attacker-controlled input: %q", err.Error())
	}
}

// TestAgentIDMintingSuffixSpellingMatchesSequenceSpelling is the anti-drift
// test validateSuffixSpelling's doc comment promises exists.
//
// validateSuffixSpelling (agentid.go) and validateSeqSpelling (messageid.go)
// are deliberate near-duplicates: they enforce the same canonical-spelling
// rule with different wording, because ID-2 shipped one and ID-3 had no reason
// to change it. Deliberate duplication only stays correct if something pins
// the two to ONE accept/reject set, which is what this does — over a shared
// table of suffix strings, ParseMessageID and ParseAgentID must AGREE on
// whether the id is valid. A change to either validator that is not made to
// the other fails here.
func TestAgentIDMintingSuffixSpellingMatchesSequenceSpelling(t *testing.T) {
	suffixes := []struct {
		desc string
		s    string
	}{
		{"one", "1"},
		{"seven", "7"},
		{"ten", "10"},
		{"max uint64", "18446744073709551615"},
		{"zero", "0"},
		{"double zero", "00"},
		{"leading zeros", "007"},
		{"leading zero", "01"},
		{"empty", ""},
		{"leading plus", "+1"},
		// "-1" is valid on BOTH sides, for the same structural reason: the
		// extra dash is absorbed by the last-dash split into the component
		// before it (a trailing-dash bus id, a trailing-dash name). Agreement
		// is what is asserted, not validity.
		{"leading minus", "-1"},
		{"leading space", " 1"},
		{"trailing space", "1 "},
		{"trailing letter", "1x"},
		{"embedded underscore", "1_0"},
		{"overflows uint64", "18446744073709551616"},
		{"fullwidth zero", "０"},
	}

	for _, tt := range suffixes {
		tt := tt
		t.Run(tt.desc, func(t *testing.T) {
			msgID := "bus-x" + "-" + tt.s
			agentID := "bus-x" + "." + "bob" + "-" + tt.s

			_, _, msgErr := ParseMessageID(msgID)
			_, _, _, agentErr := ParseAgentID(agentID)

			msgOK := msgErr == nil
			agentOK := agentErr == nil
			if msgOK != agentOK {
				t.Fatalf("suffix %q: ParseMessageID(%q) valid = %v (err %v), ParseAgentID(%q) valid = %v (err %v); the two spelling validators must accept and reject exactly the same set",
					tt.s, msgID, msgOK, msgErr, agentID, agentOK, agentErr)
			}
		})
	}
}
