package relay

import (
	"encoding/json"
	"testing"
)

// TestPeerErrorCodeAdmitsSignatureCodes proves the RELAY-9 fix:
// peerErrorCode's allow-list must admit the three SIGN-7 codes our OWN
// RelayHandler emits (relayhttp.go:311, via handshake.go:66-68) — CodeUnsigned,
// CodeBadSignature and CodeUnpeeredBus — rather than falling through to
// "unrecognised error code". Before this fix, a peer legitimately refusing a
// relayed message for a signature reason was logged as noise instead of the
// actual, operator-actionable verdict.
func TestPeerErrorCodeAdmitsSignatureCodes(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"unsigned envelope", CodeUnsigned},
		{"bad signature", CodeBadSignature},
		{"unpeered origin bus", CodeUnpeeredBus},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, err := json.Marshal(ErrorBody{Error: tc.code})
			if err != nil {
				t.Fatalf("marshal ErrorBody: %v", err)
			}
			got := peerErrorCode(buf)
			if got != tc.code {
				t.Fatalf("peerErrorCode(%q) = %q, want the code itself (%q) — a code our own RelayHandler emits must never fall through to the unrecognised-code placeholder", buf, got, tc.code)
			}
			if got == "unrecognised error code" {
				t.Fatalf("peerErrorCode(%q) fell through to the unrecognised placeholder for a code RelayHandler genuinely emits (RELAY-9 regression)", buf)
			}
		})
	}
}

// TestPeerErrorCodeStillRejectsUnknownCodes is a companion negative case: the
// allow-list must not become "admit anything" while fixing RELAY-9 — a code
// nobody in this package emits still reports as unrecognised.
func TestPeerErrorCodeStillRejectsUnknownCodes(t *testing.T) {
	buf, err := json.Marshal(ErrorBody{Error: "totally_made_up_code"})
	if err != nil {
		t.Fatalf("marshal ErrorBody: %v", err)
	}
	if got := peerErrorCode(buf); got != "unrecognised error code" {
		t.Fatalf("peerErrorCode of a genuinely unknown code = %q, want the unrecognised placeholder", got)
	}
}
