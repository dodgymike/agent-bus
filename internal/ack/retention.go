package ack

import (
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// Retention is how long ONE lifecycle row is kept: 24h, measured from
// AcceptedAt for a non-terminal row and from SettledAt for a terminal one
// (ACK-CONTRACT.md §11).
//
// # IT IS ADOPTED BY REFERENCE, AND THE REFERENCE IS THE ROOT OF THE CHAIN
//
// The contract names relay.OutboxSettledRetention. That constant is itself
// `= RetryHorizonCeiling`, which is itself `= idem.PeerOutageBudget` "so the two
// cannot drift apart" (internal/relay/forward.go:70-72,
// internal/relay/outbox.go:169). This constant therefore points at the SAME
// number through the SAME chain — it is not a second literal.
//
// # WHY THE ROOT AND NOT relay.OutboxSettledRetention DIRECTLY
//
// Two independent reasons, and both hold:
//
//  1. DIRECTION. internal/relay is where ACK-4 and ACK-5 will EMIT lifecycle
//     outcomes from, so relay -> ack is the direction this package must leave
//     open; ack -> relay would make that a cycle and force a later task to move
//     this constant under deadline.
//  2. THE IMPORT GUARD. TestRelayImportedOnlyByWiringSites
//     (internal/relay/guards_test.go) permits internal/relay to be imported by
//     the COMPOSITION SITES ONLY — internal/httpapi and cmd/agent-bus — and it
//     counts TEST files too. It refused an earlier revision of the drift test
//     that lived in an external ack_test package here. The guard was not
//     widened for a drift assertion.
//
// internal/idem is a leaf with no internal dependencies (see its doc.go), so
// pointing at the root of the chain is safe in both directions and names the
// same value.
//
// The drift that indirection invites is caught mechanically, not by trust:
// TestAckRetentionMatchesOutboxSettledRetention, in
// cmd/agent-bus/ack_retention_drift_test.go — the one place that already
// legitimately imports all three — asserts
// Retention == relay.OutboxSettledRetention and goes red if either moves. That
// is exactly the precedent idem.ParkedPollMax sets with
// TestParkedPollMaxMatchesHub.
//
// # WHY THIS TERM AND NOT A ROUNDER OR LONGER ONE
//
// Derived, not picked. Nothing can still be in flight past the outbox's own
// horizon — relay.NewForwarder REFUSES options where
// RetryHorizon + Timeout > RetryHorizonCeiling (forward.go:608-613). So a LONGER
// window retains rows nothing can change, and a SHORTER one lets a live pending
// hop outlive its own status row, which would tell a sender `unknown` about a
// delivery still in progress — worse than telling it nothing.
const Retention = idem.PeerOutageBudget

// The MEMORY BOUND, derived rather than picked — mirroring
// internal/idem/retention.go:81-152 exactly, because §11.2 says to and because
// the derivation is only worth writing down if every term is a bound the code
// actually holds to.
//
// # Where MaxRecordBytes comes from
//
// One retained Record's worst-case footprint, field by field. Every length below
// is ENFORCED by Record.validate, not merely assumed:
//
//	correlation_key  <= ids.MaxMessageIDLen                85 B
//	recipient        <= ids.MaxAgentIDLen                 150 B
//	sender           <= ids.MaxAgentIDLen                 150 B
//	state            <= len("undeliverable")               13 B
//	class            <= len("recipient_refused_not_addressed") 31 B
//	attested_by      <= len("recipient_signature_unverified") 30 B
//	accepted_at + settled_at  RFC3339Nano, 2 x 35          70 B
//	record_version                                          2 B
//	JSON field names, quoting and punctuation             ~130 B
//	map bucket + composite key + slice/struct headers     ~200 B
//	expiry-queue entries, 2 per row at most               ~112 B
//	                                                     -------
//	                                                     ~973 B  -> rounded up to 1 KiB
//
// The expiry term is NOT decoration and was added after it was pointed out as
// missing: sweepLocked's queue holds one entry per (row, anchor) — a composite
// key plus a time.Time, and at most two per row — so it is real per-row memory
// that the budget has to charge. MaxEntries is presented as DERIVED rather than
// picked, and a derivation missing a term is a number that was picked.
//
// # The term the rounding does NOT cover, stated rather than glossed
//
// The line above charges 2 LIVE entries per row (2 x 56 B = 112 B). Since
// IDEM-19 the queue also carries a DEAD PREFIX of popped, zeroed slots,
// compacted only once it reaches the size of the live suffix (Store.expiryHead,
// compactExpiryLocked). The ALLOCATION can therefore approach twice the live
// size — up to ~4 slots per row, ~224 B — which takes the worst case to ~1085 B
// and PAST the 1 KiB rounding, i.e. ~67.8 MiB against the 64 MiB budget, an
// overshoot of ~3.8 MiB.
//
// MaxRecordBytes is deliberately NOT changed for it, for the same reason its
// internal/idem twin is not: the constant divides MaxRetainedBytes to produce
// MaxEntries, which is pinned by test and is the documented cap — moving it is a
// cross-package change, not a comment correction. The dead prefix is bounded
// strictly below the live count, so the overshoot cannot grow past the figure
// above.
//
// This paragraph exists because the sentence at the top of this block claims
// every term is a bound the code actually holds to, and because the previous
// wording here — "it fits inside the existing 1 KiB rounding with room to spare"
// — became false the moment the compaction was deferred. A stale bound that
// reads as freshly checked is the defect class IDEM-19 was filed against; it
// would be absurd to leave one inside the fix for it.
//
// The arithmetic is checked by TestMaxRecordBytesBoundsWorstCase, which builds
// the LARGEST record the validators admit and asserts its encoded size fits —
// so this comment cannot quietly become a description of the happy path.
//
// # The throughput consequence, stated without softening
//
// MaxEntries rows over Retention is 65536 / 86400s ~= 0.76 rows per second,
// sustained. Since a local send creates ONE ROW PER RECIPIENT, a bus that
// sustains more than that for a full day reaches the cap, after which new rows
// are refused — the SEND still succeeds and the observation degrades to
// `unknown` (see §11.3 and Store.Accept).
const (
	// MaxStateLen, MaxClassLen and MaxAttestationLen are the longest spellings
	// in each closed enum. They are stated so the derivation above has named
	// terms rather than magic numbers, and they are asserted against the actual
	// enum members by TestEnumSpellingBounds.
	MaxStateLen       = len("undeliverable")
	MaxClassLen       = len("recipient_refused_not_addressed")
	MaxAttestationLen = len("recipient_signature_unverified")

	// maxTimestampLen is one RFC3339Nano timestamp in UTC:
	// "2026-08-16T10:24:54.357208123Z" and a little slack for the widest
	// rendering.
	maxTimestampLen = 35

	// maxIDBytes is the id half of the footprint, taken from the ids package so
	// it cannot drift from what validate enforces.
	maxIDBytes = ids.MaxMessageIDLen + 2*ids.MaxAgentIDLen

	// MaxRecordBytes is one retained row's worst-case footprint. See above.
	MaxRecordBytes = 1 << 10

	// MaxRetainedBytes is the memory budget for the whole lifecycle table. The
	// same 64 MiB budget internal/idem uses, for the same reason: it is a
	// number an operator can multiply out.
	MaxRetainedBytes = 64 << 20

	// MaxEntries is the hard cap on retained rows: 65536.
	MaxEntries = MaxRetainedBytes / MaxRecordBytes

	// PressureLine is the fill level at which the per-sender fair share starts
	// being enforced, for the DEFAULT MaxEntries: 32768.
	//
	// It is the fill level at which the table's FREE space stops exceeding its
	// USED space (count >= maxEntries - count), which is a CROSSOVER rather than
	// a round number that happens to be a half. Below it, whatever one sender
	// has consumed is by construction still available to everyone else, so the
	// rule stays entirely out of the way.
	//
	// EXPORTED FOR DOCUMENTATION, NOT FOR CALLERS: admission derives the line
	// from the STORE'S OWN maxEntries, because a bound that only exists at 65536
	// entries is a bound no test can ever demonstrate.
	PressureLine = MaxEntries / 2
)

// capacityLogInterval throttles the "table is full" ERROR line.
//
// Invariant 6 requires every discard to be logged LOUDLY AND SPECIFICALLY, and
// the first refusal after the table fills is logged unconditionally at ERROR
// with the full remedy. What is throttled is only the REPETITION: once full, a
// busy bus refuses on every send, and an unthrottled line would emit thousands
// per second — which does not make the discard more visible, it makes the log
// unreadable and pushes the first, informative line out of any retention window.
// A running counter is carried on every line and exposed by Stats, so nothing is
// lost by not printing it.
const capacityLogInterval = time.Minute
