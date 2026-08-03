// Anti-drift checks for the constants internal/idem RESTATES rather than
// imports (IDEM-11).
//
// internal/idem is a LEAF package: it has no internal dependencies, which is
// what lets every other package depend on it without a cycle. The price is that
// four numbers in internal/idem/retention.go are COPIES of numbers that live
// elsewhere, and a copy with nothing pinning it is a number that will be wrong
// eventually — silently, because nothing recomputes the derivation at runtime.
//
// retention.go's own header makes the promise these tests keep: "a term that
// stops being true should be corrected here". A promise with no test behind it
// is a comment. These are the tests.
//
// They live in internal/hub because this is an EXTERNAL test package
// (package hub_test), so it may import internal/auth, internal/ids and client
// freely — none of which internal/idem itself is allowed to touch.
package hub_test

import (
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/client"
	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// TestSessionLifetimeMaxMatchesAuth pins idem.SessionLifetimeMax, the term in
// the retention derivation that covers a client re-establishing its credential
// after an outage.
//
// If auth.SessionLifetime ever rises, the retention window stops covering a
// client that spent a full session lifetime getting back on the bus — and it
// stops covering it SILENTLY, because a too-short window does not error, it just
// applies a retry a second time.
func TestSessionLifetimeMaxMatchesAuth(t *testing.T) {
	if idem.SessionLifetimeMax != auth.SessionLifetime {
		t.Fatalf("idem.SessionLifetimeMax is %v but auth.SessionLifetime is %v; internal/idem restates this term in its retention derivation (internal/idem/retention.go) and cannot import internal/auth to check it, so this test is the only thing holding the two together. Correct retention.go — and RetentionWindow with it — rather than deleting this test",
			idem.SessionLifetimeMax, auth.SessionLifetime)
	}
}

// TestTransportRetryHorizonCoversTheClient pins idem.TransportRetryHorizon
// against the client's ACTUAL retry configuration.
//
// It asserts a bound rather than an equality, because the horizon is derived
// from three of the client's constants rather than copied from one: with
// DefaultRetryAttempts total tries there are at most attempts-1 backoff sleeps,
// and each sleep is a full-jitter draw from a window that doubles from
// BaseDelay but is clamped to MaxDelay (see client/transport.go's backoff — a
// server Retry-After can raise the window but is clamped to the same ceiling).
// So the sleeping is bounded by (attempts-1) * MaxDelay, and the horizon must
// leave room for that plus a round trip.
func TestTransportRetryHorizonCoversTheClient(t *testing.T) {
	sleeps := client.DefaultRetryAttempts - 1
	if sleeps < 0 {
		sleeps = 0
	}
	maxSleeping := time.Duration(sleeps) * client.DefaultRetryMaxDelay
	if idem.TransportRetryHorizon <= maxSleeping {
		t.Fatalf("idem.TransportRetryHorizon is %v but the client can sleep for up to %v across %d retries (client.DefaultRetryAttempts=%d, client.DefaultRetryMaxDelay=%v); the horizon must EXCEED the sleeping so a round trip still fits inside it. Correct internal/idem/retention.go — and RetentionWindow with it",
			idem.TransportRetryHorizon, maxSleeping, sleeps, client.DefaultRetryAttempts, client.DefaultRetryMaxDelay)
	}
	// The base delay only matters through the doubling, which the clamp above
	// already bounds; assert it is not somehow larger than the ceiling, which
	// would mean the first sleep alone exceeded the term.
	if client.DefaultRetryBaseDelay > client.DefaultRetryMaxDelay {
		t.Fatalf("client.DefaultRetryBaseDelay (%v) exceeds client.DefaultRetryMaxDelay (%v), so the clamp this derivation relies on no longer bounds the first sleep",
			client.DefaultRetryBaseDelay, client.DefaultRetryMaxDelay)
	}
}

// TestMaxAgentLenMatchesIDs pins idem.MaxAgentLen, the bound Record.validate
// ENFORCES so that MaxRecordBytes is a real bound rather than a description of
// the happy path.
//
// If ids.MaxAgentIDLen ever rises above this, legitimate records for
// legitimately-long agent ids start being REJECTED — on the write path that
// fails the operation loudly, but on the replay path it discards an applied key
// and a later retry is applied twice. If it ever falls, the memory derivation
// silently over-counts. Neither is detectable from internal/idem alone.
func TestMaxAgentLenMatchesIDs(t *testing.T) {
	if idem.MaxAgentLen != ids.MaxAgentIDLen {
		t.Fatalf("idem.MaxAgentLen is %d but ids.MaxAgentIDLen is %d; internal/idem restates this bound (internal/idem/retention.go) because it is a leaf package and cannot import internal/ids. A record carrying a legitimate agent id longer than idem.MaxAgentLen is rejected — on replay that silently drops an applied key and lets a later retry apply twice. Correct retention.go rather than deleting this test",
			idem.MaxAgentLen, ids.MaxAgentIDLen)
	}
}
