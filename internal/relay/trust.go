package relay

import (
	"crypto/ed25519"
	"fmt"

	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// Compile-time evidence that the durable peer store is the production
// CrossBusTrust implementation. There is no permissive/default implementation.
var _ CrossBusTrust = (*PeerStore)(nil)

// PinnedBusSigningKeys returns only the active, operator-configured pins for
// busID from the durable peer trust table.
//
// A PeerStore built without PeerStoreOptions.Dir is REFUSED even when its raw
// PinnedKeys view contains an active record. Such a store cannot consult the
// RELAY-34 withdrawal floor, so a discarded WAL tombstone could resurrect a
// revoked key. It is useful for bounded replay tests and audits, but it must
// never back an attribution decision.
func (s *PeerStore) PinnedBusSigningKeys(busID string) ([]ed25519.PublicKey, error) {
	if s == nil {
		return nil, fmt.Errorf("relay: cross-bus trust has a nil peer store")
	}
	if s.floorPath == "" {
		return nil, fmt.Errorf("relay: peer store for bus %q has no durable withdrawal floor; construct it with PeerStoreOptions.Dir before using it for verification", s.busID)
	}
	if err := ids.ValidateBusID(busID); err != nil {
		return nil, fmt.Errorf("relay: requested trust pins for an invalid bus id: %v", err)
	}
	return s.PinnedKeys(busID), nil
}

// AttestedSignerKey verifies the envelope-carried origin attestation against
// the pins supplied by VerifyRelayed and returns the exact messaging key those
// signed bytes covered.
//
// It delegates all cryptographic and binding policy to internal/attest: no
// nearest-peer re-attestation, cache, network lookup, TOFU path or fallback is
// available here. The subject's bus half is derived only after its fully-
// qualified shape has been validated; VerifyRelayed independently proves that
// half equals RelayedMessage.OriginBus before this method is reached.
func (s *PeerStore) AttestedSignerKey(fqAgentID string, originAttestation attest.Attestation, pinnedOriginBusSigningKeys []ed25519.PublicKey) (ed25519.PublicKey, error) {
	if s == nil {
		return nil, fmt.Errorf("relay: cross-bus trust has a nil peer store")
	}
	if s.floorPath == "" {
		return nil, fmt.Errorf("relay: peer store for bus %q has no durable withdrawal floor; it cannot verify origin attestations safely", s.busID)
	}
	if s.now == nil {
		return nil, fmt.Errorf("%w: peer store has no verification clock", attest.ErrNoClock)
	}
	originBus, _, _, err := ids.ParseAgentID(fqAgentID)
	if err != nil {
		return nil, fmt.Errorf("%w: subject agent id: %v", attest.ErrInvalid, err)
	}
	return attest.Verify(pinnedOriginBusSigningKeys, originAttestation, attest.Subject{
		FQAgentID: fqAgentID,
		OriginBus: originBus,
	}, s.now())
}
