package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// TestStartupRefusesNonLoopbackListenWithPeersAndInviteGateOff is RELAY-26's
// proof: the startup precondition refuses to start ONLY when all three dangerous
// conditions hold at once — a non-loopback -listen, federation configured, and
// invite-gating off — and ALLOWS the moment any one of them is false.
//
// It exercises listenExposureError directly, which is the whole check (run()
// calls it with peerRecordsExist(peerStore) and enrolmentInviteRequired). That
// keeps the combinations testable even though enrolmentInviteRequired is a
// constant true in the shipped build, so the refusal is unreachable through run()
// today — the point of factoring the decision into a pure function.
func TestStartupRefusesNonLoopbackListenWithPeersAndInviteGateOff(t *testing.T) {
	const (
		loopbackV4    = "127.0.0.1:8080"
		loopbackV6    = "[::1]:8080"
		loopbackName  = "localhost:8080"
		allInterfaces = ":8080"        // empty host: all interfaces, NON-loopback
		wildcardV4    = "0.0.0.0:8080" // NON-loopback
		routableV4    = "192.168.1.5:8080"
	)

	tests := []struct {
		name       string
		listen     string
		federation bool
		inviteGate bool
		wantRefuse bool
	}{
		// The one dangerous combination: all three true -> REFUSE.
		{"all-three-empty-host", allInterfaces, true, false, true},
		{"all-three-wildcard", wildcardV4, true, false, true},
		{"all-three-routable", routableV4, true, false, true},

		// Any single safe condition -> ALLOW.
		{"safe-loopback-v4", loopbackV4, true, false, false},
		{"safe-loopback-v6", loopbackV6, true, false, false},
		{"safe-loopback-name", loopbackName, true, false, false},
		{"safe-no-federation", routableV4, false, false, false},
		{"safe-invite-gate-on", routableV4, true, true, false},

		// Two safe conditions at once still ALLOW.
		{"safe-loopback-and-gate", loopbackV4, true, true, false},
		{"safe-no-fed-and-gate", routableV4, false, true, false},
		{"safe-loopback-no-fed", loopbackV4, false, false, false},

		// Fully benign default: loopback, no peers, gate on.
		{"benign-default", loopbackV4, false, true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := listenExposureError(tc.listen, tc.federation, tc.inviteGate)
			if tc.wantRefuse {
				if err == nil {
					t.Fatalf("listenExposureError(%q, fed=%v, gate=%v) = nil, want a refusal", tc.listen, tc.federation, tc.inviteGate)
				}
				// The error must name all three conditions and the remedy, so an
				// operator can act on it without reading the source.
				msg := err.Error()
				for _, want := range []string{"non-loopback", "invite-gating is off", "SSH tunnel", tc.listen} {
					if !strings.Contains(strings.ToLower(msg), strings.ToLower(want)) {
						t.Fatalf("refusal message %q does not mention %q", msg, want)
					}
				}
			} else if err != nil {
				t.Fatalf("listenExposureError(%q, fed=%v, gate=%v) = %v, want nil (a safe combination must start)", tc.listen, tc.federation, tc.inviteGate, err)
			}
		})
	}
}

// TestListenExposureUsesTheShippedInviteGateConstant pins the wiring: run() passes
// enrolmentInviteRequired as the invite-gating argument, and that constant is
// true, so the refusal is unreachable in the shipped build. If the constant is
// ever flipped to false this test fails, forcing a look at whether the exposure
// refusal (and invariant 3) still hold — the refusal is the safety net for
// exactly that build.
func TestListenExposureUsesTheShippedInviteGateConstant(t *testing.T) {
	if !enrolmentInviteRequired {
		t.Fatalf("enrolmentInviteRequired is false: the RELAY-26 startup refusal is now REACHABLE through run(); confirm invariant 3 and the exposure refusal are both intended for this build")
	}
	// With the shipped constant, even the otherwise-dangerous listen/federation
	// pair must be allowed, because invite-gating closes the exposure.
	if err := listenExposureError(":8080", true, enrolmentInviteRequired); err != nil {
		t.Fatalf("with invite-gating on the shipped build must start on any -listen; got %v", err)
	}
}

// TestPeerRecordsExistDetectsFederationConfig verifies condition (b): federation
// is "configured" when the peer store holds ANY peer route OR bus-trust record,
// broader than bindablePeerCount (which needs an inbound client-certificate
// binding). A nil store — federation disabled for the run — reports false.
func TestPeerRecordsExistDetectsFederationConfig(t *testing.T) {
	if peerRecordsExist(nil) {
		t.Fatalf("peerRecordsExist(nil) = true, want false (no store => no federation to protect)")
	}

	dir := t.TempDir()
	dl := &deferredLog{}
	store, err := relay.NewPeerStore(relay.PeerStoreOptions{BusID: wiringLocalBus, Dir: dir, Durable: dl})
	if err != nil {
		t.Fatalf("relay.NewPeerStore: %v", err)
	}
	walLog, err := wal.Open(wal.LogOptions{Dir: dir, Applier: store})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = walLog.Close() })
	dl.log = walLog

	if peerRecordsExist(store) {
		t.Fatalf("peerRecordsExist = true on an empty store, want false")
	}

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	// A trust record with NO inbound client-certificate binding: bindablePeerCount
	// would count 0, but this IS federation configuration and must count here.
	if _, err := store.PutTrust(relay.BusTrust{BusID: wiringPeerBus, SigningKeys: []ed25519.PublicKey{pub}}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	if bindablePeerCount(store) != 0 {
		t.Fatalf("test premise broken: bindablePeerCount should be 0 for a binding-less trust record")
	}
	if !peerRecordsExist(store) {
		t.Fatalf("peerRecordsExist = false after a bus-trust record was added, want true (broader than bindablePeerCount)")
	}
}
