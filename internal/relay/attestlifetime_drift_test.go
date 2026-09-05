package relay

import (
	"testing"

	"github.com/dodgymike/agent-bus/internal/attest"
)

// TestMaxAttestationLifetimeTracksMinter is the drift guard for RELAY-28's
// verifier-side ceiling. attest.MaxAttestationLifetime caps how far into the
// future a verifier will trust an attestation, and its derivation
// (attest.go, MaxAttestationLifetime's doc) is that the largest window an honest
// bus mints is exactly what the egress minter uses:
// NotAfter = issuedAt + RetryHorizonCeiling (cmd/agent-bus/relayegress.go), plus
// one ClockSkewAllowance for verifier clock lag.
//
// attest imports idem.PeerOutageBudget directly and RetryHorizonCeiling is
// defined as idem.PeerOutageBudget (forward.go:72), so the ceiling cannot loosen
// unless idem.PeerOutageBudget moves. This test pins the remaining link — that
// the minter's actual constant, RetryHorizonCeiling, still equals the window term
// of the ceiling — so the day someone redefines RetryHorizonCeiling to no longer
// equal idem.PeerOutageBudget, the verifier ceiling and the minter's window part
// company loudly here rather than silently in production.
//
// It lives in package relay because attest cannot import internal/relay (relay
// imports attest, and internal/relay/guards_test.go forbids the reverse), while
// relay may import attest freely.
func TestMaxAttestationLifetimeTracksMinter(t *testing.T) {
	want := RetryHorizonCeiling + attest.ClockSkewAllowance
	if attest.MaxAttestationLifetime != want {
		t.Fatalf("attest.MaxAttestationLifetime = %s, want RetryHorizonCeiling (%s) + attest.ClockSkewAllowance (%s) = %s; the verifier-side attestation lifetime ceiling has drifted from the maximum window the egress minter produces",
			attest.MaxAttestationLifetime, RetryHorizonCeiling, attest.ClockSkewAllowance, want)
	}
}
