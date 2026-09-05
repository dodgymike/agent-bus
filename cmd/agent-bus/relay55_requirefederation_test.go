package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// TestRequireFederationRefusesStartupWhenPeerSurfaceIncomplete is RELAY-55's
// proof, and the operator's explicit caution is what it is written to satisfy:
// the -require-federation gate must be a guard that CAN fire, not a RELAY-26-style
// refusal gated on a compile-time const that always short-circuits.
//
// It proves two things:
//
//  1. The refuse-condition is REACHABLE through supported configuration. It
//     stands up a REAL relay.PeerStore, adds a binding-less bus-trust record with
//     the SAME PutTrust the `agent-bus peer add` command drives, and asserts the
//     resulting state is exactly "records present, ingress absent":
//     peerRecordsExist == true while bindablePeerCount == 0. That is the third
//     case of run()'s ingress switch, where peerSurface stays nil.
//
//  2. In that reachable state the refusal ACTUALLY FIRES. requireFederationError
//     owns the whole decision (run() calls it with peerRecordsExist(peerStore)
//     and peerSurface != nil), so driving it with federationConfigured=true and
//     peerSurfaceMounted=false is driving the real production state. Deleting the
//     refusal from requireFederationError turns the "refuses" sub-test RED.
func TestRequireFederationRefusesStartupWhenPeerSurfaceIncomplete(t *testing.T) {
	t.Run("the refuse-condition is reachable: records present, ingress absent", func(t *testing.T) {
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

		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("ed25519.GenerateKey: %v", err)
		}
		// A bus-trust record with NO inbound client-certificate binding — exactly
		// what `agent-bus peer add -signing-key …` writes without
		// -peer-client-fingerprint. This is a supported, egress-capable config.
		if _, err := store.PutTrust(relay.BusTrust{BusID: wiringPeerBus, SigningKeys: []ed25519.PublicKey{pub}}); err != nil {
			t.Fatalf("PutTrust: %v", err)
		}

		// The premise the whole task turns on: records exist, but no adjacent bus
		// has an inbound binding, so the ingress surface would NOT mount.
		if !peerRecordsExist(store) {
			t.Fatalf("peerRecordsExist = false, want true after a bus-trust record was added")
		}
		if bindablePeerCount(store) != 0 {
			t.Fatalf("bindablePeerCount = %d, want 0 for a binding-less trust record: the surface must NOT mount for this to be the deaf state", bindablePeerCount(store))
		}

		// With records present and the surface unmounted (peerSurfaceMounted=false),
		// -require-federation MUST refuse. This is the guard firing on the real
		// reachable state, not on a constant.
		err = requireFederationError(true, peerRecordsExist(store), bindablePeerCount(store) > 0, dir)
		if err == nil {
			t.Fatalf("requireFederationError refused nothing on the reachable deaf state; the guard cannot fire")
		}
		msg := err.Error()
		for _, want := range []string{"require-federation", "peer records", "did not mount", "silently deaf"} {
			if !strings.Contains(strings.ToLower(msg), strings.ToLower(want)) {
				t.Fatalf("refusal message %q does not mention %q", msg, want)
			}
		}
	})

	// The full truth table. requireFederationError refuses ONLY when all three of
	// (flag on, federation configured, surface NOT mounted) hold, and ALLOWS the
	// moment any one is false — including the ordinary non-federating bus, which
	// -require-federation must not break.
	t.Run("truth table", func(t *testing.T) {
		tests := []struct {
			name       string
			require    bool
			configured bool
			mounted    bool
			wantRefuse bool
		}{
			// The one refusing combination: flag on, records present, no surface.
			{"deaf: flag on, records, no surface", true, true, false, true},

			// Flag off never refuses, even in the deaf state (default behaviour).
			{"flag off in the deaf state", false, true, false, false},

			// Surface mounted: federation is actually served, so allow.
			{"flag on, records, surface mounted", true, true, true, false},

			// No records: not deaf, just non-federating. Must start.
			{"flag on, no records, no surface", true, false, false, false},
			{"flag on, no records, surface mounted", true, false, true, false},

			// Fully benign: flag off, nothing configured.
			{"flag off, nothing configured", false, false, false, false},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				err := requireFederationError(tc.require, tc.configured, tc.mounted, "/tmp/x")
				if tc.wantRefuse && err == nil {
					t.Fatalf("requireFederationError(%v,%v,%v) = nil, want a refusal", tc.require, tc.configured, tc.mounted)
				}
				if !tc.wantRefuse && err != nil {
					t.Fatalf("requireFederationError(%v,%v,%v) = %v, want nil", tc.require, tc.configured, tc.mounted, err)
				}
			})
		}
	})
}
