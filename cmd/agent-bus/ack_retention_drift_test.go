package main

import (
	"testing"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/relay"
)

// THE ACK RETENTION DRIFT GUARD, AND WHY IT LIVES AT THE COMPOSITION ROOT
// RATHER THAN BESIDE THE CONSTANT IT GUARDS.
//
// ACK-CONTRACT.md §11 rules that the delivery lifecycle window is
// relay.OutboxSettledRetention, adopted BY REFERENCE and never as a second
// literal. internal/ack cannot import internal/relay to say so, for two
// independent reasons, and BOTH have to hold or this file would be in the wrong
// place:
//
//  1. DIRECTION. internal/relay is where ACK-4 and ACK-5 will EMIT lifecycle
//     outcomes from, so relay -> ack is the import direction that must stay
//     open. ack -> relay would make it a cycle and force a later task to move
//     this constant under deadline.
//  2. THE GUARD. TestRelayImportedOnlyByWiringSites
//     (internal/relay/guards_test.go) permits internal/relay to be imported by
//     the two COMPOSITION SITES ONLY — internal/httpapi and cmd/agent-bus — and
//     it counts test files. It caught an earlier revision of this test sitting
//     in an external ack_test package and refused it. That guard was NOT
//     widened to accommodate this: keeping the relay import surface to the
//     wiring sites is what makes "the mount is what carries a peer principal"
//     reviewable, and a drift assertion is nowhere near a good enough reason to
//     add a third directory to it.
//
// So ack.Retention is defined as idem.PeerOutageBudget — the ROOT of the chain
// relay.OutboxSettledRetention = relay.RetryHorizonCeiling = idem.PeerOutageBudget
// — and the equivalence the contract actually asks for is asserted HERE, at the
// one place that already legitimately imports all three.

// TestAckRetentionMatchesOutboxSettledRetention is ACK-CONTRACT.md §11's ruling
// stated as an assertion: the ACK window IS the per-hop tombstone window, not a
// second number that has to be kept in agreement with it.
//
// If this goes red, ONE of the constants moved. The fix is to decide which
// number is right — NEVER to update this test to match whichever changed.
func TestAckRetentionMatchesOutboxSettledRetention(t *testing.T) {
	if ack.Retention != relay.OutboxSettledRetention {
		t.Fatalf("ack.Retention = %s but relay.OutboxSettledRetention = %s; the ACK window is adopted BY REFERENCE from the per-hop tombstone window, so a difference means one of them was changed without the other",
			ack.Retention, relay.OutboxSettledRetention)
	}
	if ack.Retention != relay.RetryHorizonCeiling {
		t.Fatalf("ack.Retention = %s but relay.RetryHorizonCeiling = %s; a SHORTER window would let a live pending hop outlive its own status row, and a sender would be told `unknown` about a delivery still in progress — worse than being told nothing",
			ack.Retention, relay.RetryHorizonCeiling)
	}
	if ack.Retention != idem.PeerOutageBudget {
		t.Fatalf("ack.Retention = %s but idem.PeerOutageBudget = %s; ack.Retention is DEFINED as that constant, so this can only fail if the definition was replaced by a literal",
			ack.Retention, idem.PeerOutageBudget)
	}
}

// TestAckKindIsRegisteredAndDistinct pins the wiring this task delivers, at the
// one place that decides it.
//
// It is deliberately about the CONSTANTS rather than about run()'s applier map,
// which cannot be reached without opening a data directory: a duplicate
// discriminator is the failure that would matter, because a map literal silently
// keeps the LAST value for a repeated key and one of the two tables would then
// be replayed by the other's applier — records read as damage, in silence, which
// is the exact defect invariant 6 rates as worse than the discard.
func TestAckKindIsRegisteredAndDistinct(t *testing.T) {
	if ack.RecordKind == "" {
		t.Fatal("ack.RecordKind is empty; wal.Open refuses an empty discriminator on the way in AND on the way out")
	}
	kinds := map[string]string{
		"ack":    ack.RecordKind,
		"outbox": relay.OutboxRecordKind,
		"peer":   relay.PeerRecordKind,
		"trust":  relay.BusTrustRecordKind,
	}
	seen := make(map[string]string, len(kinds))
	for name, k := range kinds {
		if prev, dup := seen[k]; dup {
			t.Fatalf("%s and %s share the wal.Entry.Kind discriminator %q; the applier map keys on it, so one table would be replayed by the other's applier", prev, name, k)
		}
		seen[k] = name
	}
}
