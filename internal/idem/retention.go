package idem

import "time"

// The RETENTION WINDOW, derived term by term.
//
// Task IDEM-11 forbids picking a round number, and for a good reason: the
// window is the whole guarantee. "Duplicates are suppressed within the
// retention window" is only worth saying if the window demonstrably EXCEEDS the
// longest interval over which a client could still be retrying — otherwise the
// sentence is true and useless, because the case it excludes is exactly the
// case it was written for.
//
// So every term below is traceable to something in this repository, and each
// was checked against the code (not the memory of it) when this file was
// written. A term that stops being true should be corrected here, and the
// arithmetic assertion in TestRetentionWindowDerivation will fail until the
// stated total is corrected with it.
const (
	// PeerOutageBudget is the longest peer-bus outage this bus undertakes to
	// suppress duplicates across. RELAY-4 (peer-down retry/backoff) is NOT yet
	// implemented, so this is not read off an existing ceiling — it is the
	// BUDGET RELAY-4 must design within. Stated as a constraint, not an
	// observation: if RELAY-4's total retry horizon ever exceeds this, a
	// returning peer's retry falls outside the window and is applied as a new
	// operation.
	PeerOutageBudget = 24 * time.Hour

	// SessionLifetimeMax: invariant 3 caps a session at one hour. A peer or
	// client returning from an outage must re-establish a session before it can
	// retry, and the worst case is a session that expired the instant the
	// outage began — so the outage budget and a full session lifetime add
	// rather than overlap.
	SessionLifetimeMax = time.Hour

	// ParkedPollMax mirrors hub.MaxPollTimeout (5 minutes): the hard ceiling on
	// a parked long poll, whatever the client asks for. A client that was
	// parked when the partition started only begins its retry loop once the
	// poll returns.
	//
	// It is RESTATED here rather than imported because internal/idem is a leaf
	// package with no internal dependencies (see doc.go), and importing
	// internal/hub — which will import this package — would invert the
	// dependency and then cycle. The drift that restating invites is caught by
	// TestParkedPollMaxMatchesHub in internal/hub, which asserts
	// idem.ParkedPollMax == hub.MaxPollTimeout and fails if either moves.
	ParkedPollMax = 5 * time.Minute

	// TransportRetryHorizon bounds ONE client call's own retry loop.
	//
	// Verified against client/config.go and client/transport.go's backoff():
	// client.DefaultRetryAttempts is 3 and is documented as the TOTAL number of
	// tries, so there are at most 2 backoff sleeps. Each sleep is a full-jitter
	// draw from [0, window], where window doubles from
	// client.DefaultRetryBaseDelay (200ms) but is capped at
	// client.DefaultRetryMaxDelay (5s); a server Retry-After (the bus sends 5s)
	// can raise the window but is itself clamped to that same 5s cap. So the
	// worst case is 2 * 5s = 10s of sleeping, plus one round trip. Rounded UP
	// to 11s so the round trip is inside the term rather than assumed away.
	TransportRetryHorizon = 11 * time.Second

	// MaxRetryHorizon is the longest interval, end to end, over which a
	// well-behaved client or peer could still be retrying one operation: it
	// waits out an outage, re-establishes a session, drains a parked poll, and
	// then runs its own retry loop.
	MaxRetryHorizon = PeerOutageBudget + SessionLifetimeMax + ParkedPollMax + TransportRetryHorizon

	// RetentionSafetyFactor is the margin over MaxRetryHorizon. 2x rather than
	// 1x because every term above is an estimate of somebody else's behaviour,
	// and being wrong in the short direction double-applies an operation (not
	// recoverable) while being wrong in the long direction costs memory
	// (recoverable, and observable — see Stats).
	RetentionSafetyFactor = 2

	// RetentionWindow is how long an applied-key record is remembered. It is
	// 50h10m22s — deliberately not a round number, because a round number is
	// evidence that it was chosen rather than derived.
	RetentionWindow = MaxRetryHorizon * RetentionSafetyFactor
)

// The MEMORY BOUND, also derived rather than picked.
//
// # Where MaxRecordBytes comes from
//
// One retained Record's worst-case footprint, field by field:
//
//	key          <= MaxKeyLen                    128 B
//	agent        <= MaxAgentLen (== ids.MaxAgentIDLen) 150 B
//	op           <= len("peer-enrol")             ~10 B
//	fingerprint  == FingerprintSize                32 B
//	result       <= MaxResultBytes                512 B
//	CommittedAt + Seq + EnrolBusWide              ~32 B
//	map bucket + order-slice element + headers   ~150 B
//	                                            -------
//	                                            ~1014 B  ->  rounded up to 1 KiB
//
// # The throughput consequence, stated without softening
//
// MaxEntries records over RetentionWindow is
// 65536 / 180622s ~= 0.36 accepted mutating operations per second, sustained.
// A bus that sustains more than that will reach the cap and begin REFUSING
// operations with ErrCapacity until the oldest records age out.
//
// That is the deliberate fail-closed trade, not an oversight: NOTHING is
// evicted to make room, because evicting a live key turns its next retry into a
// second effect. A refused operation is recoverable; a duplicated one is not.
//
// It is also exactly why the count and the oldest-key age have to be OBSERVABLE
// rather than assumed (task point (g)): an operator needs to see the table
// filling before it refuses, and needs to see how much of the window is
// actually being used. Stats exposes both, for CORE-5's inspect endpoint to
// surface.
const (
	// MaxAgentLen bounds the agent id a Record may carry, and it is ENFORCED
	// (Record.validate), not merely assumed by the arithmetic above.
	//
	// That distinction is the whole reason this constant exists. The size
	// derivation is only worth writing down if every term in it is a bound the
	// code actually holds to: an unenforced "agent <= 150 B" line would make
	// MaxRecordBytes a description of the happy path rather than a bound, and a
	// record read off a damaged or hostile log could then carry an agent field
	// limited only by wal.MaxPayloadSize (1 MiB) — 65536 of which is three
	// orders of magnitude past the 64 MiB budget MaxEntries is derived from.
	//
	// The value is exactly ids.MaxAgentIDLen. It is RESTATED rather than
	// imported for the same reason ParkedPollMax is: internal/idem is a leaf
	// package with no internal dependencies (see doc.go). The drift that
	// restating invites is caught by TestMaxAgentLenMatchesIDs in internal/hub,
	// which may import both.
	//
	// The operation needs no companion bound: Operation is a closed enum and
	// Record.validate rejects anything outside it, so its length is bounded by
	// the constants themselves.
	MaxAgentLen = 150

	// MaxResultBytes is the largest stored result an operation may record. An
	// operation whose encoded result exceeds it fails BEFORE anything durable
	// is written (see Record.Encode), rather than being discovered at replay.
	MaxResultBytes = 512

	// MaxRecordBytes is one retained record's worst-case footprint. See the
	// arithmetic above.
	MaxRecordBytes = 1 << 10

	// MaxRetainedBytes is the memory budget for the whole applied-key table.
	MaxRetainedBytes = 64 << 20

	// MaxEntries is the hard cap on retained records: 65536. It is the quotient
	// of the two constants above, and it must EQUAL the value CONTRACTS-HTTP.md
	// already documents for hub.MaxIdempotencyEntries — hub.MaxIdempotencyEntries
	// is defined as this constant so the number cannot move in one place only.
	MaxEntries = MaxRetainedBytes / MaxRecordBytes
)
