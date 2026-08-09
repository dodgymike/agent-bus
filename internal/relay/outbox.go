package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// THE DURABLE RELAY OUTBOX — RECORD AND REPLAY (RELAY-15, part 1 of 2)
//
// Cross-bus delivery is currently BEST EFFORT and the Forwarder says so in its
// own doc comment: its queue is process memory, so everything queued for a peer
// is lost on a crash and dropped when the queue fills. Invariant 4 says nothing
// is acknowledged before it is durable; a relay hop that quietly evaporates is
// exactly the silent loss that invariant exists to forbid.
//
// This file is the DURABLE SUBSTRATE ONLY. The Forwarder is deliberately
// UNTOUCHED by this task — RELAY-19 is the task that makes forward.go write and
// settle these records. The split is not bookkeeping: a substrate that lands
// alone is reviewable alone, and a half-wired forwarder would be a worse thing
// to have in main than an unwired outbox.
//
// # WHAT A RECORD IS, AND WHAT IT IS DELIBERATELY NOT
//
// One record is one JOB: the delivery of ONE message to ONE peer bus. Two
// records share a job id and the state moves ONE WAY —
//
//	pending -> delivered
//	pending -> abandoned
//
// and never back. See upsertLocked for the monotonicity rule and why it makes
// replay order irrelevant.
//
// THE RECORD DOES NOT CARRY THE MESSAGE BODY, AND THAT IS A DECISION.
//
// It carries what invariant 6 names as routing information — message id,
// sender's bus, recipient bus, size, content hash, timestamps — and nothing
// else. Three reasons, in order of weight:
//
//  1. The body is ALREADY durable, exactly once, in this bus's own message
//     record (store.Message.Body, written through the same WAL). Writing it
//     again per peer would put up to MaxPeers copies of every relayed body on
//     disk, multiply recovery time by the same factor, and create a second
//     durable copy that can disagree with the first.
//  2. store.Message retains every field the relay envelope needs — Sender,
//     Broadcast, Recipients, BusPath, TimestampUnixMilli, Signature, Body,
//     ContentSHA256 — so RELAY-19 can rebuild a RelayRequest from the message
//     record this job names. ContentSHA256 is stored HERE as well so that the
//     rebuild can be CHECKED rather than assumed: a job whose message no longer
//     hashes to what the job was created for is refused, not sent.
//  3. It keeps the in-memory bound honest. A record is a few hundred bytes, so
//     MaxOutboxJobs is a memory bound anyone can multiply out; a record holding
//     a 256 KiB envelope would make the same cap mean four gigabytes.
//
// # THIS PACKAGE IS STILL NOT REACHABLE FROM ANY MUX
//
// Nothing here changes that: the outbox is a library the Forwarder will use, not
// a route, and it registers nothing.
//
// The MECHANISM has moved and this comment used to describe the old one. RELAY-18
// retired the blanket "nothing may import internal/relay" ban (see doc.go, "THE
// BLANKET IMPORT BAN IS RETIRED"); what is still enforced, and what actually
// matters here, is that no peer-facing route is mounted.
//
// See doc.go for the package-level gating warning.

// OutboxRecordKind is the wal.Entry.Kind discriminator for an outbox record.
//
// Entry.Kind is a FREE-FORM APPLICATION DISCRIMINATOR that sits INSIDE the
// prepare payload, above the framing layer — it is not a numbered frame type.
// So NO record-type reservation is needed for it and internal/wal/format.go's
// Type enum is not touched. This is written down, exactly as invite.RecordKind
// writes it down, so that the next reader does not go and reserve a number that
// nothing requires; CONTRACTS-ONDISK.md records that precedent for
// auth.RecordKind ("agent") and invite.RecordKind.
//
// THE "outbox" ROW ITSELF IS NOT IN CONTRACTS-ONDISK.md YET. RELAY-15 did not
// own that file (a concurrent task was editing it), so the row is reported for a
// documentation pass rather than written here. Do not read this comment as a
// claim that the kind is already documented.
const OutboxRecordKind = "outbox"

// outboxJobIDSep separates the two halves of a job id.
//
// '|' is chosen because it CANNOT occur in either half: a bus id is
// ids.BusIDPattern ([A-Za-z0-9_-]) and a message id is a bus id, a '-' and
// decimal digits. So the split is unambiguous and a job id can be read straight
// out of the WAL by an operator, which is the property the whole durable format
// is optimised for.
const outboxJobIDSep = "|"

// MaxOutboxJobIDLen bounds a job id. It is DERIVED from the two halves rather
// than chosen, so it cannot drift away from what DeriveJobID can produce.
const MaxOutboxJobIDLen = MaxPeerBusIDLen + len(outboxJobIDSep) + ids.MaxMessageIDLen

// MaxOutboxJobs is the hard cap on pending outbox jobs.
//
// The value is MaxPeers (64) x DefaultQueueDepth (256): the in-memory queue
// capacity the Forwarder already has. Settled records are tombstones rather
// than work in flight and do not consume this capacity: refusing or retiring
// one early can resurrect an already-delivered message. They remain bounded by
// OutboxSettledRetention instead.
const MaxOutboxJobs = MaxPeers * DefaultQueueDepth

// MaxOutboxRetainedJobs bounds all retained lifecycle records, including the
// terminal tombstones which prevent an acknowledged delivery being replayed.
const MaxOutboxRetainedJobs = MaxOutboxJobs

// MaxOutboxRetainedBytes is the default canonical-record budget.  The factor
// is deliberately conservative; admission computes the exact worst lifecycle
// encoding for each job rather than charging this average.
const MaxOutboxRetainedBytes = MaxOutboxRetainedJobs * 2048

// OutboxRetryHorizon is how long a PENDING job may sit before it is beyond
// saving.
//
// IT IS BOUND BY REFERENCE TO RetryHorizonCeiling, which is itself bound by
// reference to idem.PeerOutageBudget — never a duplicated literal. The chain
// matters: idem's applied-key retention promises that a peer which has been
// away for up to PeerOutageBudget still remembers the keys we are about to
// retry, so a job older than that is not merely stale, it is a job whose retry
// the receiving bus would apply as a NEW operation. Retrying past the horizon
// converts at-least-once delivery into duplicate delivery (invariant 10), which
// is worse than the loss it was trying to avoid.
//
// So a pending record older than this is DROPPED — loudly, per invariant 6,
// because a dropped pending job is a message this bus will never deliver.
//
// # IT IS THE CEILING, NOT forward.go's DERIVED HORIZON. RELAY-19 MUST SUBTRACT.
//
// forward.go sets DefaultRetryHorizon = RetryHorizonCeiling - DefaultForwardTimeout
// precisely so the LAST attempt cannot still be in flight past the budget. This
// constant is the raw ceiling, because the outbox bounds how long a RECORD is
// retained, not when an attempt is issued. A forwarder that treats a job as
// sendable right up to this boundary can therefore start a request that lands
// outside idem.PeerOutageBudget — the duplicate this horizon exists to prevent.
// RELAY-19 owns that subtraction, exactly as forward.go already does it.
const OutboxRetryHorizon = RetryHorizonCeiling

// OutboxSettledRetention is how long a SETTLED (delivered or abandoned) record
// is kept after it settles.
//
// A settled record is a TOMBSTONE: its only remaining job is to refuse a stale
// pending record for the same job id, which is what stops a delivered message
// being resurrected and sent twice. The window is the retry horizon — one more
// reference to the same constant, and one less number to keep in agreement with
// it — because a pending record older than OutboxRetryHorizon is itself dropped
// by the same sweep.
//
// SAY EXACTLY WHAT IT DOES AND DOES NOT COVER. It covers pending records ALREADY
// IN THE DURABLE LOG when the job settled. It does NOT make the job id
// permanently unusable: a FRESH Enqueue after the tombstone retires is admitted
// as a new job, which is caller-driven re-delivery and RELAY-19's to avoid (see
// sweepLocked). And it is not by itself what stops a stale SIBLING outliving the
// settlement — that is upsertLocked's table-gated Retired check, which exists
// because this window alone was proven insufficient.
const OutboxSettledRetention = RetryHorizonCeiling

// MaxOutboxReasonLen bounds the abandonment reason, measured on the VALID UTF-8
// string — which is what makes the bound meaningful, but is NOT the same number
// as the encoded length.
//
// The reason is written by THIS bus (a drop cause, an HTTP status, a peer's
// error code), but a peer's error code can reach it, so it is bounded like
// every other field that a remote party can influence: an unbounded reason
// would let a peer choose the size of our durable record and of the operator
// line we log about it.
//
// THE MEASUREMENT POINT IS THE POINT. A bound on the raw Go string is NOT a
// bound on the record, because encoding/json rewrites every invalid UTF-8 byte
// as U+FFFD — three bytes for one — so a 200-byte reason of invalid UTF-8
// encodes to 600 and Encode's own validate would then REFUSE the record. That
// refusal lands on Settle, which means the job stays PENDING and is retried
// forever: a peer able to choose those bytes could make its own jobs
// unsettleable and hold slots against the cap for the full horizon. So the
// reason is put through sanitiseOutboxReason on the way in (which makes it
// valid UTF-8 and truncates on a rune boundary), and validate additionally
// REQUIRES valid UTF-8 so a record off disk cannot smuggle the same expansion
// back in.
//
// TO BE EXACT, BECAUSE THIS FILE TRADES ON EXACT CLAIMS: JSON string escaping
// still expands what lands on disk — a control byte becomes "\u0000", six
// characters — so 256 here is up to ~1.5 KiB encoded. That is bounded and far
// under wal.MaxPayloadSize; what the UTF-8 requirement buys is that the
// expansion is bounded by a CONSTANT FACTOR at all, rather than being decided by
// bytes a peer chose.
const MaxOutboxReasonLen = 256

// OutboxReasonUnspecified is the reason stored when a caller abandons a job
// without giving one. See sanitiseOutboxReason.
const OutboxReasonUnspecified = "unspecified: the caller abandoned this job without stating a reason"

// OutboxClockSkewAllowance is how far into the future a durable timestamp may
// sit before the record is treated as damaged.
//
// It exists because the age horizon is arithmetic on a wall clock: a record
// dated in the FUTURE makes now.Sub(EnqueuedAt) negative, so Expired is
// permanently false and the job never ages out — the horizon, which is the
// thing standing between a stale job and a duplicate delivery, simply stops
// applying. The realistic cause is not corruption but an NTP step or a restored
// VM snapshot moving the clock BACKWARDS relative to records already on disk.
//
// Zero tolerance would be wrong in the other direction: a one-second backwards
// step would then discard every pending job, losing relay hops to fix a
// bookkeeping problem. Five minutes is the allowance client/clientcert.go
// already uses for exactly this class of disagreement
// (clientCertClockSkewAllowance), reused rather than re-chosen.
const OutboxClockSkewAllowance = 5 * time.Minute

// contentSHA256HexLen is the length of a lowercase hex SHA-256.
const contentSHA256HexLen = 64

// Outbox failures. All are checkable with errors.Is.
var (
	// ErrInvalidOutboxRecord reports a record that is not self-consistent: a
	// malformed id, a job id that does not match its own components, a state
	// carrying fields that state does not own.
	ErrInvalidOutboxRecord = newOutboxError("relay: invalid outbox record")

	// ErrOutboxCapacity reports the MaxOutboxJobs cap. On the live path it
	// refuses the enqueue; on the replay path it DISCARDS the record, which is
	// why the cap has to be enforced there too — see upsertLocked.
	ErrOutboxCapacity = newOutboxError("relay: outbox is at capacity")

	// ErrOutboxNotDurable reports an Outbox built without a durable log. It is
	// a REFUSAL rather than a degraded in-memory mode: an outbox that forgets on
	// restart is the exact thing this file exists to replace, and one that
	// silently degraded to it would be indistinguishable from a working one
	// until the crash that mattered.
	ErrOutboxNotDurable = newOutboxError("relay: outbox has no durable log")

	// ErrOutboxUnknownJob reports a settle for a job id the outbox does not
	// hold.
	ErrOutboxUnknownJob = newOutboxError("relay: unknown outbox job")

	// ErrOutboxSettled reports an attempt to move a job that has already
	// reached a terminal state — the resurrection this table refuses.
	ErrOutboxSettled = newOutboxError("relay: outbox job is already settled")

	// ErrOutboxExpired reports a job past OutboxRetryHorizon.
	ErrOutboxExpired = newOutboxError("relay: outbox job is past the retry horizon")

	// ErrOutboxInFlight reports a second concurrent transition for one job.
	ErrOutboxInFlight = newOutboxError("relay: outbox job already has a transition in flight")

	// ErrOutboxSelfAddressed reports a job whose destination is this bus.
	ErrOutboxSelfAddressed = newOutboxError("relay: outbox job is addressed to this bus")
)

// outboxError keeps this file's sentinels distinct from the package's other
// error families without importing a new dependency for it.
type outboxError struct{ msg string }

func newOutboxError(msg string) *outboxError { return &outboxError{msg: msg} }

func (e *outboxError) Error() string { return e.msg }

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

// OutboxState is where a job is in its lifecycle. It is a CLOSED enum: a value
// outside these three is rejected by validate in both directions.
type OutboxState uint8

const (
	// OutboxPending is a job this bus still owes a peer. It is the ONLY state
	// that puts a job in the pending set replay rebuilds.
	OutboxPending OutboxState = iota + 1
	// OutboxDelivered is a job the peer accepted (or settled as a final,
	// non-retriable outcome such as a loop drop). Terminal.
	OutboxDelivered
	// OutboxAbandoned is a job this bus gave up on: the horizon ran out, the
	// peer answered finally and negatively, or the job was discarded. Terminal,
	// and it MUST carry a reason — a message this bus will never deliver is
	// exactly the discard invariant 6 requires be recorded specifically rather
	// than silently.
	OutboxAbandoned
)

// String returns the wire spelling. It is what goes on disk, so it is a fixed
// string and not a number: a numeric enum in a durable record is unreadable to
// the operator interpreting the log with `head -c` and a pretty-printer, and it
// silently changes meaning if the constants are ever reordered.
func (s OutboxState) String() string {
	switch s {
	case OutboxPending:
		return "pending"
	case OutboxDelivered:
		return "delivered"
	case OutboxAbandoned:
		return "abandoned"
	default:
		return fmt.Sprintf("OutboxState(%d)", uint8(s))
	}
}

// Terminal reports whether the state is absorbing. See upsertLocked: this is
// the property the monotonicity rule is written in terms of.
func (s OutboxState) Terminal() bool {
	return s == OutboxDelivered || s == OutboxAbandoned
}

// parseOutboxState maps the wire spelling back onto a state. An unrecognised
// value is an ERROR, never a default: guessing here would turn a corrupt or
// future-format record into a plausible-looking pending job, and a fabricated
// pending job is a duplicate delivery.
func parseOutboxState(s string) (OutboxState, error) {
	switch s {
	case "pending":
		return OutboxPending, nil
	case "delivered":
		return OutboxDelivered, nil
	case "abandoned":
		return OutboxAbandoned, nil
	default:
		return 0, fmt.Errorf("%w: state %q is not one of pending, delivered, abandoned", ErrInvalidOutboxRecord, elideOutbox(s))
	}
}

// ---------------------------------------------------------------------------
// The record
// ---------------------------------------------------------------------------

// DeriveJobID mints the job id for one (peer, message) delivery.
//
// IT IS A PURE FUNCTION OF SERVER-VALIDATED INPUTS, and that is what makes the
// outbox idempotent: enqueueing the same message for the same peer twice names
// the SAME job, so the second enqueue finds the first rather than creating a
// duplicate delivery. Invariant 1 is satisfied because both halves are ids this
// side already validated — a peer bus id we peered with and a message id whose
// authority is the origin bus — and nothing a client supplies reaches it
// unvalidated.
//
// It does not validate; Encode and DecodeOutboxRecord do, and a decoded record
// is re-checked against this function so a record whose id disagrees with its
// own components is treated as corruption.
//
// IT IS CASE-SENSITIVE, while the rest of the system treats bus ids
// case-insensitively — registry.go keys peers on strings.ToLower, path.go folds
// every hop for loop prevention, ValidatePeerBusID compares with EqualFold, and
// minted bus ids are lowercase by construction. So two case-variant spellings
// are ONE bus everywhere else, and here they would mint TWO job ids and deliver
// the message twice.
//
// TWO EARLIER RATIONALES FOR LEAVING IT ARE RECORDED AS WRONG, so nobody
// re-derives them. It is NOT that folding would "change an id that is already
// durable" — nothing is durable yet, the package is unwired, and this is the
// cheapest moment to change it. Nor is it that folding would collide two
// genuinely distinct origin buses: per the above, case-variant bus ids are not
// distinct anywhere else in this system.
//
// The real reason to defer is MECHANICAL. validate re-derives the job id from
// the record's STORED PeerBusID, so folding inside this function would make
// every mixed-case record fail its own integrity check and be discarded as
// "names one job and describes another" — a self-inflicted relay-hop loss. The
// correct form normalises at the ENQUEUE BOUNDARY, before the record is built,
// which is a decision about the canonical spelling of a durable field and
// belongs with the task that wires the forwarder. RELAY-19 owns it.
func DeriveJobID(peerBusID, originMessageID string) string {
	return peerBusID + outboxJobIDSep + originMessageID
}

// OutboxRecord is one durable relay delivery job.
//
// EVERY DURABLE ENTRY CARRIES THE COMPLETE RECORD IN ITS POST-TRANSITION STATE,
// never a delta — the discipline internal/invite's record follows, for the same
// two reasons:
//
//   - replay needs no ordering logic beyond a monotonic upsert, so there is no
//     second mechanism that could disagree with the live path;
//   - if an EARLIER record for the same job is lost (a corrupt frame, a
//     capacity discard), a surviving LATER record still reconstructs the job in
//     its SETTLED state. Under a delta scheme the same loss would leave the job
//     looking PENDING — the one direction that produces a second delivery.
type OutboxRecord struct {
	// JobID is DeriveJobID(PeerBusID, OriginMessageID). It is stored rather
	// than only derived so the record is self-describing in the log — and
	// because storing it turns the redundancy into an INTEGRITY CHECK: validate
	// re-derives it and refuses a record whose id does not match its own
	// components, which is the shape a spliced or forged record has.
	JobID string

	// PeerBusID is the bus this job owes the message to. It is a peer, never
	// this bus: Outbox.Enqueue checks that through ValidatePeerBusID, because a
	// job addressed to ourselves is a loop with a durable record attached.
	PeerBusID string

	// OriginMessageID is the ORIGIN bus's "<bus-id>-<seq>" id for the message
	// (invariant 1). It identifies WHAT is owed, it is the relay idempotency
	// key on the wire (invariant 10, ValidateRelayRequest's protocol rule), and
	// its bus half is the origin bus — so no separate origin-bus field exists,
	// because two durable fields that must agree are two fields that can
	// disagree.
	OriginMessageID string

	// Size is the message body's length in bytes, and ContentSHA256 is its hex
	// SHA-256 — the two quantitative facts about content invariant 6 keeps in a
	// routing record. Together they let RELAY-19 verify that the message it
	// rebuilds the envelope from is the message this job was created for,
	// instead of trusting a message id to still mean the same bytes.
	Size          int
	ContentSHA256 string

	// EnqueuedAt is when the job was accepted into the outbox, by THIS bus's
	// clock. It is the anchor of the age horizon and it is not decoration:
	// anchoring on enqueue rather than on the first or last attempt is what
	// keeps the total retry horizon inside idem.PeerOutageBudget when jobs queue
	// up behind a dead peer — the same choice relayJob.enqueuedAt makes, for the
	// same reason.
	EnqueuedAt time.Time

	// State is the lifecycle state. The two fields below are valid if and only
	// if State names their event; validate enforces that in both directions.
	State OutboxState

	// SettledAt is when the job reached a terminal state. Set iff State is
	// terminal, and it is the input to OutboxSettledRetention, so a settled
	// record without one could never be swept.
	SettledAt time.Time

	// Reason is why the job was abandoned. REQUIRED on abandoned and forbidden
	// on every other state: an abandoned job is a message this bus will never
	// deliver, and invariant 6 is explicit that the defect is the SILENT
	// discard, not the discard. A record that cannot say why is a silent one
	// with a timestamp.
	Reason string
}

// outboxRecordJSON is the wire shape: compact, no HTML escaping, omitempty on
// everything optional, the state a fixed STRING and times RFC3339Nano in UTC —
// so an operator can read a job straight out of the WAL.
type outboxRecordJSON struct {
	JobID           string `json:"job_id"`
	PeerBusID       string `json:"peer_bus"`
	OriginMessageID string `json:"origin_message_id"`
	Size            int    `json:"size"`
	ContentSHA256   string `json:"content_sha256"`
	EnqueuedAt      string `json:"enqueued_at"`
	State           string `json:"state"`
	SettledAt       string `json:"settled_at,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// OriginBus is the bus that accepted the message from its own agent: the bus
// half of OriginMessageID.
//
// It is DERIVED rather than stored, which is why it is a method. validate has
// already proved the id parses, so a parse failure here is impossible for any
// record that came out of Encode or DecodeOutboxRecord; it returns "" rather
// than panicking on the impossible case.
func (r OutboxRecord) OriginBus() string {
	busID, _, err := ids.ParseMessageID(r.OriginMessageID)
	if err != nil {
		return ""
	}
	return busID
}

// Expired reports whether a PENDING job has sat longer than the horizon.
//
// It is a PURE PREDICATE over EnqueuedAt and the clock, and it is the ONLY
// definition of the age horizon in this file — the live path and the replay
// path both call it, so they cannot drift into disagreeing about which jobs are
// still worth sending. "Expired" is deliberately NOT a stored state, for the
// reason internal/invite writes out at length: a stored flag is a snapshot of
// one clock reading that starts disagreeing with the clock the moment it is
// written, and keeping it true would mean a sweep REWRITING records into an
// append-only log.
//
// A terminal record is never "expired": it is retired instead, on a different
// anchor. See Retired.
func (r OutboxRecord) Expired(now time.Time, horizon time.Duration) bool {
	if r.State != OutboxPending {
		return false
	}
	return now.Sub(r.EnqueuedAt) > horizon
}

// FutureDated reports whether a PENDING record is stamped further ahead of the
// clock than the skew allowance.
//
// IT IS A PURE PREDICATE AND IT IS THE ONLY DEFINITION, called by BOTH
// upsertLocked (admission) and sweepLocked (the live table) — the same
// discipline Expired follows, and for a reason found the hard way: an earlier
// version checked skew ONLY at admission, so the replay path discarded a
// future-dated record while the live table kept one forever. A job enqueued
// under a clock a month fast was still pending ten days past its 24h horizon,
// because Expired's subtraction stays negative and nothing re-checked it. Two
// paths with different answers about which jobs are live is exactly the
// divergence deriving one predicate prevents.
//
// It does not apply to terminal records. A future SettledAt only makes a
// tombstone live LONGER, and discarding a tombstone is the resurrection
// direction — see upsertLocked's skew guard for the bug that taught us that.
func (r OutboxRecord) FutureDated(now time.Time, allowance time.Duration) bool {
	if r.State != OutboxPending {
		return false
	}
	return r.EnqueuedAt.After(now.Add(allowance))
}

// Retired reports whether a SETTLED record has outlived its usefulness as a
// tombstone. See OutboxSettledRetention for why the window is what it is.
func (r OutboxRecord) Retired(now time.Time, retention time.Duration) bool {
	if !r.State.Terminal() {
		return false
	}
	return now.Sub(r.SettledAt) > retention
}

// validate checks a record is self-consistent.
//
// IT RUNS IN BOTH DIRECTIONS, and both matter for different reasons. On the way
// OUT (Encode, before the durable write) a record that cannot be stored fails
// the operation with NOTHING written, rather than being discovered at replay
// when the effect is already durable and every remaining option is bad. On the
// way IN (DecodeOutboxRecord) a record read off disk is UNTRUSTED INPUT
// (invariant 1) even though this server wrote it — because "this server wrote
// it" is exactly the claim corruption disproves.
func (r OutboxRecord) validate() error {
	if len(r.JobID) > MaxOutboxJobIDLen {
		// Bounded BEFORE anything quotes it: the id is file-derived text of
		// unbounded length until this check has run, and an operator's log must
		// not be sizeable by whoever wrote the damaged bytes.
		return fmt.Errorf("%w: job id is %d bytes, but a job id is at most %d; it is not echoed here because it is oversized", ErrInvalidOutboxRecord, len(r.JobID), MaxOutboxJobIDLen)
	}
	if len(r.PeerBusID) > MaxPeerBusIDLen {
		return fmt.Errorf("%w: peer bus id is %d bytes, but a bus id is at most %d; it is not echoed here because it is oversized", ErrInvalidOutboxRecord, len(r.PeerBusID), MaxPeerBusIDLen)
	}
	if err := ids.ValidateBusID(r.PeerBusID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOutboxRecord, err)
	}
	if len(r.OriginMessageID) > ids.MaxMessageIDLen {
		return fmt.Errorf("%w: origin message id is %d bytes, but a message id is at most %d; it is not echoed here because it is oversized", ErrInvalidOutboxRecord, len(r.OriginMessageID), ids.MaxMessageIDLen)
	}
	if _, _, err := ids.ParseMessageID(r.OriginMessageID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOutboxRecord, err)
	}
	// THE INTEGRITY CHECK the stored job id exists for. A record whose id does
	// not derive from its own components names one job while describing another
	// — which is how a forged or spliced record would try to settle a job it
	// does not describe, or hide a pending job under a settled job's id.
	if want := DeriveJobID(r.PeerBusID, r.OriginMessageID); r.JobID != want {
		return fmt.Errorf("%w: job id does not derive from the record's own peer bus and origin message id; the record names one job and describes another", ErrInvalidOutboxRecord)
	}
	if r.Size < 0 || r.Size > store.MaxBodyBytes {
		return fmt.Errorf("%w: size is %d, but a message body is 0..%d bytes", ErrInvalidOutboxRecord, r.Size, store.MaxBodyBytes)
	}
	if err := validateContentSHA256(r.ContentSHA256); err != nil {
		return err
	}
	if r.EnqueuedAt.IsZero() {
		return fmt.Errorf("%w: enqueued_at is the zero time, but the age horizon is computed from it, so a record without one could never expire", ErrInvalidOutboxRecord)
	}
	switch r.State {
	case OutboxPending:
		// A PENDING record carries NO terminal fields at all. Checked field by
		// field rather than trusted: a record that said "pending" while carrying
		// a settlement is exactly the shape a resurrection wants.
		if !r.SettledAt.IsZero() {
			return fmt.Errorf("%w: a pending job carries a settled_at", ErrInvalidOutboxRecord)
		}
		if r.Reason != "" {
			return fmt.Errorf("%w: a pending job carries a reason (%d bytes)", ErrInvalidOutboxRecord, len(r.Reason))
		}
	case OutboxDelivered:
		if err := r.mustBeSettled(); err != nil {
			return err
		}
		if r.Reason != "" {
			// A delivered job has nothing to explain, and a reason on one would
			// be the only place the two terminal states could be confused.
			return fmt.Errorf("%w: a delivered job carries a reason (%d bytes); only an abandoned job has one", ErrInvalidOutboxRecord, len(r.Reason))
		}
	case OutboxAbandoned:
		if err := r.mustBeSettled(); err != nil {
			return err
		}
		if r.Reason == "" {
			return fmt.Errorf("%w: an abandoned job must record WHY; a message this bus will never deliver is a discard, and invariant 6 requires the discard be recorded specifically rather than silently", ErrInvalidOutboxRecord)
		}
		if len(r.Reason) > MaxOutboxReasonLen {
			return fmt.Errorf("%w: reason is %d bytes, but a reason is at most %d; it is not echoed here because it is oversized", ErrInvalidOutboxRecord, len(r.Reason), MaxOutboxReasonLen)
		}
		if !utf8.ValidString(r.Reason) {
			// Checked rather than tolerated: encoding/json expands each invalid
			// byte to a three-byte U+FFFD, so a reason that passed the length
			// bound as raw bytes would fail it after encoding — and the failure
			// would land on the SETTLE, leaving the job pending forever. See
			// MaxOutboxReasonLen.
			return fmt.Errorf("%w: reason is not valid UTF-8; it is not echoed here", ErrInvalidOutboxRecord)
		}
	default:
		return fmt.Errorf("%w: %s is not one of the fixed lifecycle states", ErrInvalidOutboxRecord, r.State)
	}
	return nil
}

// mustBeSettled enforces the fields every terminal state owns.
func (r OutboxRecord) mustBeSettled() error {
	if r.SettledAt.IsZero() {
		return fmt.Errorf("%w: a %s job must record settled_at, and tombstone retention is computed from it", ErrInvalidOutboxRecord, r.State)
	}
	if r.SettledAt.Before(r.EnqueuedAt) {
		return fmt.Errorf("%w: a %s job settled at %s, before it was enqueued at %s", ErrInvalidOutboxRecord, r.State,
			r.SettledAt.UTC().Format(time.RFC3339Nano), r.EnqueuedAt.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// sanitiseOutboxReason coerces a caller-supplied reason into something that can
// ALWAYS be stored, so a settle can never fail because of its own explanation.
//
// A settle that fails leaves the job PENDING and therefore retried, which is the
// opposite of what the caller asked for and is reachable by a peer whose error
// text ends up here. So the reason is repaired rather than rejected: invalid
// UTF-8 is replaced (which is what encoding/json would do anyway, but HERE,
// where the length bound can still be applied to the result), and the string is
// truncated on a RUNE boundary so truncation cannot itself manufacture an
// invalid sequence.
func sanitiseOutboxReason(reason string) string {
	if !utf8.ValidString(reason) {
		reason = strings.ToValidUTF8(reason, string(utf8.RuneError))
	}
	// AN EMPTY OR BLANK REASON BECOMES THE PLACEHOLDER RATHER THAN AN ERROR.
	// validate REQUIRES a reason on an abandoned record (invariant 6: the
	// discard is never silent), so an empty one would fail the settle and leave
	// the job PENDING and retried forever — reachable the moment RELAY-19 builds
	// a reason from a peer's response and the peer returns an empty body. The
	// placeholder is not informative, but it is honest about being uninformative
	// and it settles the job; validate's rule still binds every record read off
	// disk.
	// TRUNCATE FIRST, THEN JUDGE EMPTINESS. The other order has a hole: 260
	// spaces followed by a letter is not blank, but truncating it to 256 leaves
	// 256 spaces — a reason that satisfies validate's non-empty rule and tells
	// an operator nothing, which meets invariant 6's letter and misses its
	// point.
	if len(reason) > MaxOutboxReasonLen {
		cut := MaxOutboxReasonLen
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut]
	}
	if strings.TrimSpace(reason) == "" {
		return OutboxReasonUnspecified
	}
	return reason
}

// validateContentSHA256 checks the hash is exactly 64 LOWERCASE hex characters.
//
// Lowercase is required rather than normalised: the hash is compared for
// equality on the replay path (a record claiming a job id with a different
// content hash is refused), and two spellings of one hash would make that
// comparison miss.
func validateContentSHA256(h string) error {
	if len(h) != contentSHA256HexLen {
		return fmt.Errorf("%w: content_sha256 is %d characters, want %d", ErrInvalidOutboxRecord, len(h), contentSHA256HexLen)
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return fmt.Errorf("%w: content_sha256 is not lowercase hex at offset %d", ErrInvalidOutboxRecord, i)
	}
	return nil
}

// Encode renders the record as the opaque JSON that rides in wal.Entry.Body.
//
// IT VALIDATES BEFORE IT RETURNS, so a record that cannot be stored fails the
// operation with nothing written. See validate.
func (r OutboxRecord) Encode() (json.RawMessage, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	j := outboxRecordJSON{
		JobID:           r.JobID,
		PeerBusID:       r.PeerBusID,
		OriginMessageID: r.OriginMessageID,
		Size:            r.Size,
		ContentSHA256:   r.ContentSHA256,
		EnqueuedAt:      r.EnqueuedAt.UTC().Format(time.RFC3339Nano),
		State:           r.State.String(),
	}
	// The terminal fields are written ONLY for the state that owns them, so the
	// encoder cannot produce a record its own validate would refuse on the way
	// back in.
	if r.State.Terminal() {
		j.SettledAt = r.SettledAt.UTC().Format(time.RFC3339Nano)
	}
	if r.State == OutboxAbandoned {
		j.Reason = r.Reason
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(j); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidOutboxRecord, err)
	}
	// Encoder.Encode terminates with a newline; the carrier is length-delimited
	// and needs no terminator.
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// DecodeOutboxRecord parses an outbox record read back off disk.
//
// It is STRICT in exactly the way wal.decodePayload and invite.DecodeRecord
// are: unknown fields are refused, trailing data is refused, and every field is
// re-validated. A lenient decoder here would reinstate a job with a mangled
// state or a mangled hash — and the worst of those failures reinstates a
// DELIVERED job as a pending one, which is a second delivery.
func DecodeOutboxRecord(b []byte) (OutboxRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var j outboxRecordJSON
	if err := dec.Decode(&j); err != nil {
		// ELIDED, not %v. encoding/json quotes the offending field name back at
		// you verbatim, and time.ParseError quotes its value TWICE, so a damaged
		// record with a 200 KiB unknown field produces a 200-400 KiB error
		// string. The bound-before-quote discipline this file applies to every
		// record FIELD has to apply to the wrapped stdlib error as well, or the
		// discipline has a hole exactly where the input is least trusted.
		return OutboxRecord{}, fmt.Errorf("%w: %s", ErrInvalidOutboxRecord, elideOutbox(err.Error()))
	}
	if dec.More() {
		return OutboxRecord{}, fmt.Errorf("%w: trailing data after the record", ErrInvalidOutboxRecord)
	}
	state, err := parseOutboxState(j.State)
	if err != nil {
		return OutboxRecord{}, err
	}
	enqueuedAt, err := parseOutboxTime("enqueued_at", j.EnqueuedAt)
	if err != nil {
		return OutboxRecord{}, err
	}
	r := OutboxRecord{
		JobID:           j.JobID,
		PeerBusID:       j.PeerBusID,
		OriginMessageID: j.OriginMessageID,
		Size:            j.Size,
		ContentSHA256:   j.ContentSHA256,
		EnqueuedAt:      enqueuedAt,
		State:           state,
		Reason:          j.Reason,
	}
	if j.SettledAt != "" {
		if r.SettledAt, err = parseOutboxTime("settled_at", j.SettledAt); err != nil {
			return OutboxRecord{}, err
		}
	}
	if err := r.validate(); err != nil {
		return OutboxRecord{}, err
	}
	return r, nil
}

// parseOutboxTime decodes one RFC3339Nano timestamp, normalised to UTC.
func parseOutboxTime(field, v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		// Both the value AND the wrapped error are elided: time.ParseError
		// embeds the full value a second time, so quoting it with %v would undo
		// the elision applied to v.
		return time.Time{}, fmt.Errorf("%w: %s (%q) is not RFC3339Nano: %s", ErrInvalidOutboxRecord, field, elideOutbox(v), elideOutbox(err.Error()))
	}
	return t.UTC(), nil
}

// maxElidedOutboxChars bounds how much untrusted, file-derived text may appear
// in an error string, the same discipline wal's CorruptError applies.
const maxElidedOutboxChars = 64

// elideOutbox truncates untrusted text for inclusion in an error message.
func elideOutbox(s string) string {
	if len(s) <= maxElidedOutboxChars {
		return s
	}
	// Truncated on a RUNE boundary, the same way the sibling peerstore's
	// elidePeerText does it: cutting mid-rune would put an invalid UTF-8
	// fragment in an operator's log, which %q then renders as an escape nobody
	// can read back to the original bytes.
	cut := maxElidedOutboxChars
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(truncated)"
}

// ---------------------------------------------------------------------------
// The outbox
// ---------------------------------------------------------------------------

// OutboxDurableLog is the two-phase write path the outbox commits through.
//
// wal.Log.Write runs the whole prepare -> fsync -> commit -> fsync -> Apply
// cycle and returns only once the entry is on stable storage, so an operation
// that returns a nil error here is DURABLE. Nothing in this file acknowledges
// anything before that (invariant 4).
type OutboxDurableLog interface {
	Write(wal.Entry) (wal.Committed, error)
}

type outboxCheckpointer interface{ Checkpoint() error }

// OutboxOptions configures NewOutbox. Every zero value means "the derived
// default", so a caller with no opinion gets the derivation rather than an
// accidental zero window.
type OutboxOptions struct {
	// BusID is THIS bus's id. It is required: Enqueue checks the destination
	// against it through ValidatePeerBusID, and a job addressed to ourselves is
	// a loop that would be retried until its horizon ran out.
	BusID string

	// Durable is the two-phase write path. A nil Durable makes every mutating
	// operation fail with ErrOutboxNotDurable — see that error for why this is
	// a refusal and not a degraded in-memory mode.
	Durable OutboxDurableLog

	// Logger receives every discard, every refused transition and every swept
	// job. It may be nil.
	Logger *logging.Logger

	// Now supplies the clock every predicate here is evaluated against. nil
	// means time.Now.
	Now func() time.Time

	// MaxJobs is the hard cap on pending jobs across all peers. 0 means
	// MaxOutboxJobs. Settled tombstones do not consume this capacity.
	MaxJobs int
	// MaxPendingPerPeer is the hard cap on pending jobs for one destination
	// peer, including concurrent enqueues still being fsynced. 0 derives an
	// equal share of MaxJobs across MaxPeers (at least one). The default is
	// DefaultQueueDepth.
	MaxPendingPerPeer int
	// MaxRetainedJobs and MaxRetainedBytes bound pending records plus terminal
	// tombstones. Admission reserves the worst canonical lifecycle encoding so
	// Settle never needs to perform (and can never fail) a capacity check.
	MaxRetainedJobs  int
	MaxRetainedBytes int
	// MaxRetainedPerPeer is the fair per-destination share of retained records.
	// Zero derives an equal share of MaxRetainedJobs across MaxPeers.
	MaxRetainedPerPeer int
	// RetryHorizon is the pending-job age horizon. 0 means OutboxRetryHorizon.
	RetryHorizon time.Duration
	// SettledRetention is the tombstone window. 0 means OutboxSettledRetention.
	SettledRetention time.Duration
}

// Outbox is the durable, bounded relay delivery table: enqueue, settle, replay.
//
// Three properties make the implementation safe, and they are the same three
// internal/invite's Store documents — deliberately, because matching an
// established pattern here is worth more than a cleaner design of our own:
//
// # 1. Apply is the ONLY writer on the replay path, and the live path folds in
// # the SAME canonical record
//
// Every mutating operation encodes a COMPLETE post-transition record, writes it
// through Durable, and then folds the identical record into memory. The record
// folded in is DecodeOutboxRecord(Encode(rec)) — literally the bytes replay will
// read — so a live Apply and a replayed Apply cannot drift.
//
// # 2. THE LOCK IS NEVER HELD ACROSS A DURABLE WRITE
//
// wal.Log.Write calls Applier.Apply synchronously, and this Outbox may itself
// be (or be reached from) that Applier, so holding mu across Durable.Write
// would self-deadlock the moment the log is wired to apply live commits. Every
// mutating method takes the lock, decides, RELEASES it, writes, and takes it
// again to fold the result in. What makes that safe is that no decision is left
// unguarded: capacity is reserved in pendingWrites and a per-job transition is
// reserved in inflight, so nothing slips through the window.
//
// # 3. The upsert is MONOTONIC
//
// pending -> delivered and pending -> abandoned, and nothing else. See
// upsertLocked.
type Outbox struct {
	// mu guards every field below. A plain Mutex rather than an RWMutex: every
	// exported entry point sweeps first, and sweeping mutates.
	mu sync.Mutex
	// checkpointMu serializes publication candidates. Snapshot records the exact
	// generation-scoped omissions and terminal encodings that Checkpoint may act
	// on after publication; later sweeps cannot enlarge that immutable set.
	checkpointMu sync.Mutex

	busID              string
	durable            OutboxDurableLog
	log                *logging.Logger
	now                func() time.Time
	maxJobs            int
	maxPendingPerPeer  int
	maxRetainedJobs    int
	maxRetainedBytes   int
	maxRetainedPerPeer int
	retryHorizon       time.Duration
	settledRetention   time.Duration

	// jobs is the table, keyed by job id.
	jobs map[string]OutboxRecord
	// pendingJobs and pendingByPeer count only work still owed. Tombstones stay
	// in jobs for idempotency but never consume pending capacity.
	pendingJobs   int
	pendingByPeer map[string]int

	// pendingWrites counts enqueues that have passed the capacity check but
	// whose record is not yet in the table (they are mid-fsync). It is counted
	// against the cap so that N concurrent enqueues cannot all pass a check only
	// one of them had room for, and it guarantees the post-write fold always has
	// a slot — a record that is already durable must never be refused by the
	// in-memory bound.
	pendingWrites int
	// pendingWritesByPeer makes the per-peer quota a reservation too: a slow
	// fsync cannot let concurrent enqueues for one peer race past its share.
	pendingWritesByPeer map[string]int

	// retained* includes records in the serving table and reservations whose
	// enqueue is mid-fsync. reservationBytes deliberately remains the enqueue's
	// worst lifecycle size after settlement; only a successful checkpoint may
	// rebase/reclaim it.
	retainedJobs         int
	retainedBytes        int
	retainedByPeer       map[string]int
	reservationBytes     map[string]int
	retainedWriteJobs    int
	retainedWriteBytes   int
	retainedWritesByPeer map[string]int
	// expired records are omitted from the next snapshot but remain charged and
	// present until checkpoint publication succeeds.
	expired             map[string]struct{}
	checkpointCandidate *outboxCheckpointCandidate
	restoredHighWater   uint64

	// inflight holds at most ONE lifecycle transition per job, so two concurrent
	// settles cannot both build a terminal record from the same pending one.
	inflight map[string]struct{}
}

// NewOutbox builds an empty outbox.
func NewOutbox(o OutboxOptions) (*Outbox, error) {
	if err := ids.ValidateBusID(o.BusID); err != nil {
		return nil, fmt.Errorf("relay: outbox needs this bus's own id, so a job addressed to ourselves can be refused: %w", err)
	}
	ob := &Outbox{
		busID:                o.BusID,
		durable:              o.Durable,
		log:                  o.Logger,
		now:                  o.Now,
		maxJobs:              o.MaxJobs,
		maxPendingPerPeer:    o.MaxPendingPerPeer,
		maxRetainedJobs:      o.MaxRetainedJobs,
		maxRetainedBytes:     o.MaxRetainedBytes,
		maxRetainedPerPeer:   o.MaxRetainedPerPeer,
		retryHorizon:         o.RetryHorizon,
		settledRetention:     o.SettledRetention,
		jobs:                 make(map[string]OutboxRecord),
		pendingByPeer:        make(map[string]int),
		pendingWritesByPeer:  make(map[string]int),
		retainedByPeer:       make(map[string]int),
		reservationBytes:     make(map[string]int),
		retainedWritesByPeer: make(map[string]int),
		expired:              make(map[string]struct{}),
		inflight:             make(map[string]struct{}),
	}
	if ob.log == nil {
		// The same nil-logger convention NewForwarder uses, rather than a nil
		// check at every call site: a discard logger cannot be the reason a
		// discard goes unlogged.
		ob.log = logging.New(io.Discard, logging.LevelError)
	}
	if ob.now == nil {
		ob.now = time.Now
	}
	if ob.maxJobs <= 0 {
		ob.maxJobs = MaxOutboxJobs
	}
	if ob.maxPendingPerPeer <= 0 {
		ob.maxPendingPerPeer = ob.maxJobs / MaxPeers
		if ob.maxPendingPerPeer == 0 {
			ob.maxPendingPerPeer = 1
		}
	}
	if ob.maxPendingPerPeer > ob.maxJobs {
		return nil, fmt.Errorf("relay: outbox MaxPendingPerPeer (%d) exceeds MaxJobs (%d); one peer's share cannot exceed the global pending-work bound", ob.maxPendingPerPeer, ob.maxJobs)
	}
	if ob.maxRetainedJobs <= 0 {
		ob.maxRetainedJobs = MaxOutboxRetainedJobs
	}
	if ob.maxRetainedBytes <= 0 {
		ob.maxRetainedBytes = MaxOutboxRetainedBytes
	}
	if ob.maxRetainedPerPeer <= 0 {
		ob.maxRetainedPerPeer = ob.maxRetainedJobs / MaxPeers
		if ob.maxRetainedPerPeer == 0 {
			ob.maxRetainedPerPeer = 1
		}
	}
	if ob.maxRetainedPerPeer > ob.maxRetainedJobs {
		return nil, fmt.Errorf("relay: outbox MaxRetainedPerPeer (%d) exceeds MaxRetainedJobs (%d)", ob.maxRetainedPerPeer, ob.maxRetainedJobs)
	}
	if ob.retryHorizon <= 0 {
		ob.retryHorizon = OutboxRetryHorizon
	}
	if ob.settledRetention <= 0 {
		ob.settledRetention = OutboxSettledRetention
	}
	// THE ANTI-RESURRECTION ARGUMENT RESTS ON THIS INEQUALITY, so it is enforced
	// STRUCTURALLY rather than documented — the same treatment NewForwarder
	// gives its own ceiling.
	//
	// A tombstone exists to refuse a stale pending record for the same job. If
	// it is retired FIRST, that pending record hits the not-present branch of
	// upsertLocked and is INSERTED: a delivered job is back in the pending set
	// and the peer receives the message twice. The defaults satisfy the
	// inequality (both are RetryHorizonCeiling), but a caller setting only
	// RetryHorizon would leave SettledRetention on its default and silently
	// build exactly that outbox — which is why a partially filled options struct
	// must be refused rather than trusted.
	if ob.settledRetention < ob.retryHorizon {
		return nil, fmt.Errorf("relay: outbox SettledRetention (%s) is shorter than RetryHorizon (%s). "+
			"A settled record is the tombstone that refuses a stale pending record for the same job, so retiring it first "+
			"lets a DELIVERED job be replayed back into the pending set and the message is delivered twice. "+
			"Raise SettledRetention to at least the retry horizon, or leave both at their defaults",
			ob.settledRetention, ob.retryHorizon)
	}
	return ob, nil
}

// OutboxJob is a request to remember one delivery durably.
//
// It is deliberately the ROUTING FACTS ONLY — see the file comment for why the
// body is not here and where RELAY-19 reads it back from.
type OutboxJob struct {
	// PeerBusID is the destination peer bus.
	PeerBusID string
	// OriginMessageID is the origin bus's id for the message, which is also the
	// relay idempotency key.
	OriginMessageID string
	// Size and ContentSHA256 describe the body without carrying it.
	Size          int
	ContentSHA256 string
}

// Enqueue durably records that this bus owes PeerBusID a message.
//
// It returns only once the pending record is on stable storage (invariant 4).
//
// IT IS IDEMPOTENT ON THE JOB ID. A second Enqueue for the same (peer, message)
// finds the first and returns it, writing NOTHING — which is the whole reason
// the job id is derived rather than minted fresh.
//
// The one exception is a CONCURRENT duplicate: a caller that arrives while the
// first enqueue is still mid-fsync gets ErrOutboxInFlight, because the first
// record does not exist yet for it to be handed. That is RETRYABLE, not
// terminal, and a caller must treat it as such — retrying it once the first
// write lands returns the original record by the normal idempotent path.
//
// # A NIL ERROR MEANS "DURABLE", NOT "IN THE PENDING SET". SAY SO.
//
// The two are the same in every ordinary case and come apart in one: if the
// clock moves backwards by more than OutboxClockSkewAllowance during this call's
// own fsync, the record is durable but future-dated relative to the corrected
// clock, so the fold drops it (see foldIn) and Pending() will not list it.
// Enqueue still returns (record, nil), because the record IS on stable storage
// and invariant 4 is about exactly that.
//
// Two consequences for RELAY-19, recorded here rather than discovered there:
//
//   - a caller must not treat a nil error as proof the job is queued. If it
//     needs that, it should read Pending() or Lookup rather than infer it.
//   - a job dropped this way — and a job dropped by the horizon sweep — leaves
//     an enqueue record in the WAL with NO settlement beside it, so the durable
//     trail shows an unresolved job. That is pre-existing behaviour of the
//     horizon sweep rather than something this path introduced, and closing it
//     means writing a durable abandonment from OUTSIDE the lock, which is the
//     same follow-up sweepLocked already names.
//
// A second Enqueue for a job that has already SETTLED is ErrOutboxSettled:
// re-queueing a delivered message is the resurrection that turns at-least-once
// into at-least-twice.
func (ob *Outbox) Enqueue(job OutboxJob) (OutboxRecord, error) {
	if ob.durable == nil {
		return OutboxRecord{}, ErrOutboxNotDurable
	}
	// The destination is checked against OUR id before anything else: a job
	// addressed to ourselves is a loop, and the durable record would make it a
	// loop that survives a restart.
	if err := ValidatePeerBusID(ob.busID, job.PeerBusID); err != nil {
		return OutboxRecord{}, err
	}

	now := ob.now().UTC()
	rec := OutboxRecord{
		JobID:           DeriveJobID(job.PeerBusID, job.OriginMessageID),
		PeerBusID:       job.PeerBusID,
		OriginMessageID: job.OriginMessageID,
		Size:            job.Size,
		ContentSHA256:   job.ContentSHA256,
		EnqueuedAt:      now,
		State:           OutboxPending,
	}
	// Canonicalised BEFORE the table is touched, so the record folded into
	// memory is byte-identical to the one replay will read back.
	canon, body, err := canonicalOutboxRecord(rec)
	if err != nil {
		return OutboxRecord{}, err
	}
	reservation, err := outboxLifecycleReservation(canon)
	if err != nil {
		return OutboxRecord{}, err
	}

	ob.mu.Lock()
	ob.sweepLocked(now)
	if existing, ok := ob.jobs[canon.JobID]; ok {
		ob.mu.Unlock()
		if existing.State.Terminal() {
			return OutboxRecord{}, fmt.Errorf("%w: job %s is %s (settled at %s); re-queueing it would deliver the message a second time",
				ErrOutboxSettled, existing.JobID, existing.State, existing.SettledAt.UTC().Format(time.RFC3339Nano))
		}
		// THE CONTENT IS CHECKED HERE TOO, so this path cannot be laxer than the
		// replay path — upsertLocked refuses a record claiming different content
		// for the same message id, and a live caller that did the same thing
		// would otherwise be handed the ORIGINAL job silently and believe its
		// new content was queued.
		if existing.ContentSHA256 != job.ContentSHA256 || existing.Size != job.Size {
			return OutboxRecord{}, fmt.Errorf("%w: job %s is already queued for content %s (%d bytes); a re-enqueue naming different content for the same message id is refused, because a message id is minted once and never reused",
				ErrInvalidOutboxRecord, existing.JobID, existing.ContentSHA256, existing.Size)
		}
		// A legitimate re-enqueue of a job already owed. Nothing is written and
		// the ORIGINAL enqueue time is kept, so the age horizon fires from the
		// first attempt and can never be pushed out by retrying.
		return existing, nil
	}
	// THE RESERVATION IS TAKEN, NOT MERELY CHECKED. Reading inflight without
	// writing it made this check decorative: two concurrent enqueues of the SAME
	// job both passed it and both wrote a durable PENDING record, and the second
	// one is refused by the table forever after — so every future recovery logs
	// a DISCARD at ERROR for a record that was never wrong, on the exact line
	// invariant 6 reserves for a genuinely lost relay hop.
	if _, busy := ob.inflight[canon.JobID]; busy {
		ob.mu.Unlock()
		return OutboxRecord{}, fmt.Errorf("%w: job %s", ErrOutboxInFlight, canon.JobID)
	}
	if ob.pendingJobs+ob.pendingWrites >= ob.maxJobs {
		held, writing := ob.pendingJobs, ob.pendingWrites
		ob.mu.Unlock()
		return OutboxRecord{}, fmt.Errorf("%w: %d jobs are pending and %d more are being written, against a global pending-work limit of %d; this message is NOT queued for %s and will not be relayed. Nothing is evicted to make room, because an evicted job is a message silently never delivered",
			ErrOutboxCapacity, held, writing, ob.maxJobs, canon.PeerBusID)
	}
	peerHeld := ob.pendingByPeer[canon.PeerBusID]
	peerWriting := ob.pendingWritesByPeer[canon.PeerBusID]
	if peerHeld+peerWriting >= ob.maxPendingPerPeer {
		ob.mu.Unlock()
		return OutboxRecord{}, fmt.Errorf("%w: peer %s has %d jobs pending and %d more being written, against its pending-work limit of %d; this message is NOT queued. Another peer's capacity remains available",
			ErrOutboxCapacity, canon.PeerBusID, peerHeld, peerWriting, ob.maxPendingPerPeer)
	}
	if ob.retainedJobs+ob.retainedWriteJobs >= ob.maxRetainedJobs ||
		ob.retainedBytes+ob.retainedWriteBytes+reservation > ob.maxRetainedBytes {
		heldJobs, heldBytes := ob.retainedJobs+ob.retainedWriteJobs, ob.retainedBytes+ob.retainedWriteBytes
		ob.mu.Unlock()
		return OutboxRecord{}, fmt.Errorf("%w: retained lifecycle capacity is %d jobs/%d bytes against limits %d/%d; this message is NOT queued and no tombstone is evicted",
			ErrOutboxCapacity, heldJobs, heldBytes, ob.maxRetainedJobs, ob.maxRetainedBytes)
	}
	if ob.retainedByPeer[canon.PeerBusID]+ob.retainedWritesByPeer[canon.PeerBusID] >= ob.maxRetainedPerPeer {
		held := ob.retainedByPeer[canon.PeerBusID] + ob.retainedWritesByPeer[canon.PeerBusID]
		ob.mu.Unlock()
		return OutboxRecord{}, fmt.Errorf("%w: peer %s retains %d lifecycle records against its fair limit of %d",
			ErrOutboxCapacity, canon.PeerBusID, held, ob.maxRetainedPerPeer)
	}
	// The slot AND the job id are reserved across the fsync, so neither the
	// bound nor a concurrent enqueue of the same job can be raced.
	ob.pendingWrites++
	ob.pendingWritesByPeer[canon.PeerBusID]++
	ob.retainedWriteJobs++
	ob.retainedWriteBytes += reservation
	ob.retainedWritesByPeer[canon.PeerBusID]++
	ob.reservationBytes[canon.JobID] = reservation
	ob.inflight[canon.JobID] = struct{}{}
	ob.mu.Unlock()

	// THE RELEASE IS DEFERRED SO IT RUNS AFTER foldIn, NOT BEFORE IT. Releasing
	// the reservation first would reopen the exact window it exists to close: a
	// concurrent Enqueue could take the freed slot between the decrement and the
	// fold, and this record — which is ALREADY DURABLE — would then be refused
	// by the in-memory bound. Same ordering as invite.Store.Mint, for the same
	// reason.
	defer func() {
		ob.mu.Lock()
		ob.pendingWrites--
		ob.pendingWritesByPeer[canon.PeerBusID]--
		if ob.pendingWritesByPeer[canon.PeerBusID] == 0 {
			delete(ob.pendingWritesByPeer, canon.PeerBusID)
		}
		ob.retainedWriteJobs--
		ob.retainedWriteBytes -= reservation
		ob.retainedWritesByPeer[canon.PeerBusID]--
		if ob.retainedWritesByPeer[canon.PeerBusID] == 0 {
			delete(ob.retainedWritesByPeer, canon.PeerBusID)
		}
		if _, ok := ob.jobs[canon.JobID]; !ok {
			delete(ob.reservationBytes, canon.JobID)
		}
		delete(ob.inflight, canon.JobID)
		ob.mu.Unlock()
	}()

	if _, err := ob.durable.Write(wal.Entry{Kind: OutboxRecordKind, Body: body}); err != nil {
		// NOTHING was acknowledged and nothing is in memory.
		return OutboxRecord{}, fmt.Errorf("relay: writing the outbox record for job %s: %w", canon.JobID, err)
	}
	ob.foldIn(canon, "enqueue")
	return canon, nil
}

// Settle durably moves a pending job to a terminal state.
//
// state must be OutboxDelivered or OutboxAbandoned, and an abandonment must
// carry a reason (invariant 6: the discard is recorded specifically, never
// silently). It returns only once the terminal record is on stable storage.
//
// A settle that repeats an ALREADY-APPLIED settlement verbatim is idempotent
// and writes nothing; one that contradicts it is ErrOutboxSettled and the FIRST
// settlement stands.
func (ob *Outbox) Settle(jobID string, state OutboxState, reason string) (OutboxRecord, error) {
	if ob.durable == nil {
		return OutboxRecord{}, ErrOutboxNotDurable
	}
	if !state.Terminal() {
		return OutboxRecord{}, fmt.Errorf("%w: %s is not a terminal state; a settle moves a job OUT of pending", ErrInvalidOutboxRecord, state)
	}
	// REPAIRED HERE, BEFORE ANYTHING COMPARES IT. A settle must never fail
	// because of its own explanation (the job would stay pending and be retried
	// forever), and the idempotent-repeat check below compares against the
	// STORED reason — which is the sanitised one — so comparing raw input would
	// report a verbatim repeat as a contradiction.
	if state == OutboxDelivered {
		// A delivered job has nothing to explain; a reason on one is dropped
		// rather than refused, so a caller passing a stale string cannot make a
		// delivery unrecordable.
		reason = ""
	} else {
		reason = sanitiseOutboxReason(reason)
	}

	now := ob.now().UTC()

	ob.mu.Lock()
	ob.sweepLocked(now)
	existing, ok := ob.jobs[jobID]
	if !ok {
		ob.mu.Unlock()
		return OutboxRecord{}, fmt.Errorf("%w: %s", ErrOutboxUnknownJob, elideOutbox(jobID))
	}
	if existing.State.Terminal() {
		ob.mu.Unlock()
		if existing.State == state && existing.Reason == reason {
			// The same settlement, repeated. Nothing is re-applied.
			return existing, nil
		}
		return OutboxRecord{}, fmt.Errorf("%w: job %s is already %s; a contradicting %s settlement is refused and the first is kept",
			ErrOutboxSettled, existing.JobID, existing.State, state)
	}
	if _, busy := ob.inflight[jobID]; busy {
		ob.mu.Unlock()
		return OutboxRecord{}, fmt.Errorf("%w: job %s", ErrOutboxInFlight, jobID)
	}
	ob.inflight[jobID] = struct{}{}
	ob.mu.Unlock()

	defer func() {
		ob.mu.Lock()
		delete(ob.inflight, jobID)
		ob.mu.Unlock()
	}()

	// The COMPLETE post-transition record, not a delta: every field the pending
	// record carried, plus the settlement.
	settled := existing
	settled.State = state
	settled.SettledAt = now
	settled.Reason = reason

	// THE SETTLEMENT TIME IS CLAMPED FORWARD TO THE ENQUEUE TIME, on the LIVE
	// path only.
	//
	// validate refuses SettledAt < EnqueuedAt, and rightly so for a record read
	// off disk — a job cannot settle before it exists. But on this path the
	// ordering is known CAUSALLY, not from the clock: the enqueue's own durable
	// write completed before this call could have found the job. So a `now`
	// earlier than EnqueuedAt does not mean the record is wrong, it means the
	// WALL CLOCK stepped backwards mid-flight, and taking it at face value would
	// make Encode refuse the record and the settle FAIL — leaving the job
	// pending and retried forever, which is exactly the failure mode the reason
	// sanitiser exists to prevent, arriving through a different door.
	//
	// The clamp is bounded and honest: it can only move the timestamp forward to
	// an instant this bus already recorded, it never invents one, and it never
	// applies to a record coming back off disk.
	if settled.SettledAt.Before(settled.EnqueuedAt) {
		ob.log.Warn("the clock moved BACKWARDS between an outbox job being enqueued and being settled; the settlement is recorded at the enqueue time so the record stays storable",
			"job_id", settled.JobID, "clock", now.UTC().Format(time.RFC3339Nano),
			"enqueued_at", settled.EnqueuedAt.UTC().Format(time.RFC3339Nano))
		settled.SettledAt = settled.EnqueuedAt
	}
	canon, body, err := canonicalOutboxRecord(settled)
	if err != nil {
		return OutboxRecord{}, err
	}
	if _, err := ob.durable.Write(wal.Entry{Kind: OutboxRecordKind, Body: body}); err != nil {
		return OutboxRecord{}, fmt.Errorf("relay: writing the %s record for job %s: %w", state, canon.JobID, err)
	}
	ob.foldIn(canon, "settle")
	return canon, nil
}

// Lookup returns the record for a job id.
func (ob *Outbox) Lookup(jobID string) (OutboxRecord, bool) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.sweepLocked(ob.now())
	r, ok := ob.jobs[jobID]
	if _, expired := ob.expired[jobID]; expired {
		return OutboxRecord{}, false
	}
	return r, ok
}

// Pending returns THE PENDING SET: every job this bus still owes a peer, in a
// deterministic order (oldest enqueue first, job id as the tie-break).
//
// This is what recovery rebuilds and what RELAY-19 will re-queue at startup.
// The order is deterministic rather than map order because a restart must
// re-offer jobs in the same sequence it would have sent them, and because a
// non-deterministic order makes a failure impossible to reproduce.
func (ob *Outbox) Pending() []OutboxRecord {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.sweepLocked(ob.now())
	out := make([]OutboxRecord, 0, len(ob.jobs))
	for id, r := range ob.jobs {
		if _, expired := ob.expired[id]; expired {
			continue
		}
		if r.State == OutboxPending {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EnqueuedAt.Equal(out[j].EnqueuedAt) {
			return out[i].EnqueuedAt.Before(out[j].EnqueuedAt)
		}
		return out[i].JobID < out[j].JobID
	})
	return out
}

// Len reports how many records the table retains, pending and settled.
func (ob *Outbox) Len() int {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.sweepLocked(ob.now())
	return ob.len()
}

// len is the unswept count. The caller must hold mu.
func (ob *Outbox) len() int { return len(ob.jobs) }

// put and del are the ONLY mutations of the table, so pending capacity cannot
// drift from the serving copy when a job settles, is swept, or is replayed.
// The caller must hold mu.
func (ob *Outbox) put(id string, r OutboxRecord) {
	old, existed := ob.jobs[id]
	if existed && old.State == OutboxPending {
		ob.removePending(old.PeerBusID)
	}
	if !existed {
		reserved := ob.reservationBytes[id]
		if reserved == 0 {
			var err error
			if r.State == OutboxPending {
				reserved, err = outboxLifecycleReservation(r)
			} else {
				var body json.RawMessage
				body, err = r.Encode()
				reserved = len(body)
			}
			if err != nil {
				ob.log.Error("could not account a valid outbox record", "job_id", id, "err", err)
				reserved = wal.MaxPayloadSize
			}
			ob.reservationBytes[id] = reserved
		}
		ob.retainedJobs++
		ob.retainedBytes += reserved
		ob.retainedByPeer[r.PeerBusID]++
	}
	ob.jobs[id] = r
	if existed && old.State == OutboxPending && r.State.Terminal() {
		// Settlement starts a fresh tombstone retention window. A pending record
		// previously marked for checkpoint omission must not cause this newer,
		// correctness-critical terminal state to be omitted at the same high-water.
		delete(ob.expired, id)
	}
	if r.State == OutboxPending {
		ob.pendingJobs++
		ob.pendingByPeer[r.PeerBusID]++
	}
}

func (ob *Outbox) del(id string) {
	if old, ok := ob.jobs[id]; ok {
		if old.State == OutboxPending {
			ob.removePending(old.PeerBusID)
		}
		ob.retainedJobs--
		ob.retainedBytes -= ob.reservationBytes[id]
		ob.retainedByPeer[old.PeerBusID]--
		if ob.retainedByPeer[old.PeerBusID] == 0 {
			delete(ob.retainedByPeer, old.PeerBusID)
		}
	}
	delete(ob.jobs, id)
	delete(ob.reservationBytes, id)
	delete(ob.expired, id)
}

func (ob *Outbox) removePending(peerBusID string) {
	ob.pendingJobs--
	ob.pendingByPeer[peerBusID]--
	if ob.pendingByPeer[peerBusID] == 0 {
		delete(ob.pendingByPeer, peerBusID)
	}
}

// ---------------------------------------------------------------------------
// Replay
// ---------------------------------------------------------------------------

// Apply implements wal.Applier: it folds one committed outbox entry into the
// serving copy, on replay at Open and — if the server wires this outbox as the
// log's Applier — on live commits too. It cannot tell the two apart, and must
// not need to: the record is complete in both cases.
//
// Entries of any other Kind are skipped SILENTLY: this log carries messages,
// roster entries, invites and outbox jobs, and an applier that treated its
// neighbours' records as damage would fill the log with false alarms.
//
// # APPLY MUST NEVER RETURN A NON-NIL ERROR
//
// From a live write a non-nil error POISONS the log (wal.ErrDiverged). On the
// recovery path wal/replay.go DISCARDS the entry and counts it as acknowledged
// loss rather than failing Open — invariant 6 settled that recovery ALWAYS
// reaches a running server — so an error returned there does not stop the boot,
// it silently drops a job and calls it recovered. So every failure path here — an undecodable record,
// an invalid one, the capacity bound, the age horizon, a non-monotonic
// transition — LOGS LOUDLY AND SPECIFICALLY at ERROR and returns nil. SILENT
// discard is the defect, not discard itself.
//
// A discard here is FAIL-SAFE IN THE DIRECTION THAT MATTERS. Dropping a PENDING
// record loses a relay hop, which costs availability and is logged as the data
// loss it is. Dropping a SETTLED record could in principle leave a delivered job
// looking pending — which is why the settled record is never dropped in favour
// of a pending one (upsertLocked) and why a pending record past the horizon is
// discarded rather than re-sent.
func (ob *Outbox) Apply(c wal.Committed) error {
	if c.Entry.Kind != OutboxRecordKind {
		return nil
	}
	rec, err := DecodeOutboxRecord(c.Entry.Body)
	if err != nil {
		ob.log.Error("DISCARDING an outbox record that could not be decoded; if it was a pending job, that relay hop is LOST and the message will not reach the peer",
			"prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex, "err", err)
		return nil
	}
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if c.CommitIndex <= ob.restoredHighWater {
		return nil
	}
	if err := ob.upsertLocked(rec, ob.now(), "replay"); err != nil {
		ob.log.Error("DISCARDING an outbox record that could not be applied; the job keeps the state already in memory",
			"prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex,
			"job_id", rec.JobID, "peer_bus", rec.PeerBusID, "state", rec.State.String(), "err", err)
	} else {
		ob.logCapacityDebtLocked("replay")
	}
	return nil
}

// foldIn applies a record produced by a LIVE, already-durable operation.
//
// A refusal is logged at ERROR and swallowed: the record is on stable storage,
// so the durable log is the truth and a restart rebuilds memory from it.
// Returning an error would tell the caller an operation failed that demonstrably
// did not.
// IT TAKES A FRESH CLOCK READING, AND THE ALTERNATIVE WAS TRIED AND REMOVED.
//
// An earlier revision passed in the reading the record was stamped from, to stop
// a clock step during the fsync making the fold refuse the caller's own durable
// record. Once sweepLocked learned the same FutureDated predicate as admission,
// that parameter stopped changing the OUTCOME — a record admitted by a stale
// reading is swept by the next fresh one, so both orderings converge on the same
// table — and all it still bought was which log line appeared. A parameter whose
// only remaining justification is a log line is one a later reader deletes as
// redundant, and it reads as a correctness mechanism while being none, so it is
// gone rather than left as a trap.
//
// What that costs, said plainly: a backwards clock step larger than
// OutboxClockSkewAllowance during an enqueue's fsync loses that relay hop. It is
// LOGGED, and replay reaches the same verdict, so memory and disk agree about
// which jobs are live — which is the property that actually matters. A lost hop
// is the deliberate trade against a job that can never age out and would be
// retried past idem.PeerOutageBudget as a duplicate.
func (ob *Outbox) foldIn(r OutboxRecord, source string) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if err := ob.upsertLocked(r, ob.now(), source); err != nil {
		ob.log.Error("an outbox record is DURABLE but was refused by the in-memory table; the durable log is the truth and a restart will rebuild from it",
			"job_id", r.JobID, "peer_bus", r.PeerBusID, "state", r.State.String(), "source", source, "err", err)
	}
}

// upsertLocked is the ONE place anything enters or changes in the table, and it
// is MONOTONIC.
//
// # WHAT MONOTONICITY IS KEYED ON, AND WHY REPLAY ORDER IS THEREFORE SAFE
//
// It is keyed on the STATE RANK OF THE JOB ID — pending below terminal — and on
// nothing else. Not on a sequence number, not on a timestamp, not on the
// record's position in the log. That choice is what makes the rule survive a
// log replayed in any order, or with holes in it:
//
//   - TERMINAL IS ABSORBING. delivered/abandoned never becomes pending again,
//     whatever order the records arrive in. A stale pending record replayed
//     AFTER a delivered one is REFUSED — which is precisely the case that would
//     otherwise resurrect a delivered message and turn at-least-once delivery
//     into at-least-twice.
//   - A DUPLICATE IS A NO-OP. The same record applied twice (the log replayed
//     twice, or a live fold that Apply already performed) is idempotent, because
//     the transition it asks for has already happened. ONE EXCEPTION, narrowed
//     rather than left as a blanket claim: re-applying a content-MISMATCHED
//     settlement finds the job already terminal, so it misses the merge branch
//     below and is refused by the content check. The table is unchanged — the
//     job stays settled with the first content — but the refusal is logged, so
//     the second application is a no-op in EFFECT and not in silence.
//   - THE FIRST TERMINAL RECORD WINS. Two contradicting settlements (delivered
//     and abandoned for one job) cannot both be true; keeping the first keeps
//     the record that matches what was actually done with the message, and the
//     second is refused and logged.
//
// The consequence worth stating plainly: for the records of ONE job, applying
// them in any order converges on the same final state — terminal if any
// terminal record survived, pending only if none did. So this table does not
// depend on wal replaying in commit order, even though it does.
//
// THE CLAIM IS NARROWED TO THAT, DELIBERATELY, BECAUSE TWO THINGS DO DEPEND ON
// ORDER and pretending otherwise would be the more dangerous comment:
//
//   - THE CAPACITY CAP IS ORDER-SENSITIVE. Which records get in depends on
//     which arrived while there was room. In particular a terminal record
//     discarded by the cap, followed later (after a sweep freed space) by that
//     job's pending record, resurrects the job. That is unreachable under
//     commit-order replay — a job's pending record always precedes its
//     settlement in a well-formed log, so the tombstone is never the one
//     discarded — but it is the reason the cap is not a second monotonicity
//     mechanism and must never be relied on as one.
//   - TWO DIFFERENT PENDING RECORDS for one job: the FIRST ONE APPLIED wins,
//     not the one with the earlier EnqueuedAt. Enqueue's inflight reservation
//     is what stops two from being written in the first place.
//   - RETENTION IS A THIRD ORDER DEPENDENCY, and naming only the first two is
//     what let the reproduced P0 through. The retired-tombstone gate below reads
//     TABLE STATE, so it protects a settlement only against siblings that
//     PRECEDE it in the log. Apply a tombstone and then a stale pending record
//     for the same job and the job is pending again — the gate applies the
//     tombstone and the very next sweep retires it.
//
// SO THE GUARANTEE IS EXACTLY PER-JOB COMMIT ORDER, WHICH IS WHAT wal PROVIDES:
// a job's pending records precede its settlement in the log. That is the
// assumption every argument in this file is entitled to make, and it should be
// stated rather than left as an unexamined "any order".
//
// Allowed: an insert; a re-apply of the same record; pending -> delivered;
// pending -> abandoned. Refused: everything else.
//
// The caller must hold mu.
func (ob *Outbox) upsertLocked(r OutboxRecord, now time.Time, source string) error {
	ob.sweepLocked(now)

	// A DESTINATION OF THIS BUS IS REFUSED ON THE REPLAY PATH TOO. Enqueue
	// checks it through ValidatePeerBusID, but DecodeOutboxRecord cannot — it
	// holds no local bus id. This method does, so the check belongs here as
	// well: a data directory reopened under a different -bus-id would otherwise
	// replay jobs addressed to ourselves straight into the pending set, and the
	// forwarder would retry a loop until its horizon ran out.
	if strings.EqualFold(r.PeerBusID, ob.busID) {
		return fmt.Errorf("%w: job %s is addressed to this bus (%s); it is a loop, and a durable one", ErrOutboxSelfAddressed, r.JobID, ob.busID)
	}

	// A FUTURE-DATED PENDING RECORD DISABLES THE HORIZON IT ANCHORS. Expired is
	// a subtraction against the clock, so a pending record dated ahead of now is
	// never expired — the age horizon, which is what stands between a stale job
	// and a duplicate delivery, silently stops applying and the job becomes
	// immortal. Discarded loudly rather than clamped: clamping would invent an
	// enqueue time this bus never recorded, and the honest reading of a
	// future-dated record is that either the clock or the record is wrong.
	//
	// # IT APPLIES TO PENDING RECORDS ONLY, AND THAT RESTRICTION IS THE WHOLE
	// # POINT — AN EARLIER VERSION OF THIS GUARD WAS THE BUG IT NOW AVOIDS
	//
	// The first version checked EVERY record, and a second branch discarded a
	// terminal record whose SettledAt was ahead of the clock. Both ran before
	// the state machine, so they REFUSED TOMBSTONES — the one direction this
	// file exists to prevent. A backwards clock step S with an enqueue-to-settle
	// gap G resurrected any job with 5m < S <= G+5m: the pending record was
	// inside the allowance and admitted, its delivered record was outside it and
	// discarded, and the peer got the message twice.
	//
	// So the trade is taken deliberately and in the other direction. A
	// future-dated tombstone is NOT discarded: EnqueuedAt is not the retirement
	// anchor, and a future SettledAt only makes the tombstone live LONGER.
	//
	// STATE THE COST HONESTLY, BECAUSE IT IS BIGGER THAN IT FIRST LOOKS. It is
	// one retained TOMBSTONE per job settled while the clock was fast, it
	// survives restart (a terminal record has no skew guard on replay either,
	// deliberately), and nothing bounds how far ahead SettledAt may sit — a
	// clock a week fast retains those records for a week after it is corrected.
	// Tombstones no longer consume pending capacity, so this is a memory cost,
	// not a relay-throughput outage.
	//
	// It is still the right trade: that is a self-healing memory increase on a
	// mis-set clock, against a PERMANENT duplicate delivery. Neither timestamp
	// is reachable by a peer, so it is not remotely triggerable.
	// Bounding it properly means clamping the RETIREMENT ANCHOR at admission
	// rather than discarding the record, which is a behaviour change with its
	// own memory-versus-disk question and belongs to RELAY-19, not here.
	if r.FutureDated(now, OutboxClockSkewAllowance) {
		return fmt.Errorf("%w: enqueued_at is %s, more than %s ahead of the clock (%s); a future-dated pending record can never expire, so the retry horizon would not apply to it",
			ErrInvalidOutboxRecord, r.EnqueuedAt.UTC().Format(time.RFC3339Nano), OutboxClockSkewAllowance, now.UTC().Format(time.RFC3339Nano))
	}

	// THE AGE HORIZON IS ENFORCED HERE, WHICH MEANS ON THE REPLAY PATH TOO.
	// A horizon applied only when a job is enqueued is not a horizon: a log
	// written before a long outage replays pending jobs whose retry would land
	// outside idem.PeerOutageBudget, where the receiving bus has forgotten the
	// applied key and would take the retry as a NEW message.
	if r.Expired(now, ob.retryHorizon) {
		return fmt.Errorf("%w: enqueued %s ago, past the %s horizon (idem.PeerOutageBudget); the message is DROPPED rather than retried, because a retry past the horizon is applied by the peer as a NEW message",
			ErrOutboxExpired, now.Sub(r.EnqueuedAt).Truncate(time.Second), ob.retryHorizon)
	}
	existing, ok := ob.jobs[r.JobID]

	// A RETIRED TOMBSTONE IS DROPPED ONLY WHEN THERE IS NOTHING LEFT TO SETTLE.
	//
	// THIS CHECK USED TO RUN ABOVE THE TABLE LOOKUP, AND THAT WAS A REPRODUCED
	// RESURRECTION (P0). The argument for putting it there was: retiring the
	// tombstone needs now-SettledAt > settledRetention, and SettledAt >=
	// EnqueuedAt, so now-EnqueuedAt > settledRetention >= retryHorizon and the
	// job's pending record must already be expired.
	//
	// THE FLAW IS THAT SettledAt >= EnqueuedAt IS A WITHIN-ONE-RECORD RULE
	// (mustBeSettled) BEING USED FOR A CROSS-RECORD CONCLUSION. Two pending
	// records can exist for one job with DIFFERENT anchors, and the tombstone is
	// anchored on only one of them. That is not hypothetical — sweepLocked
	// creates it: a record written under a fast clock is swept when the clock is
	// corrected, the caller re-offers the message, and the log ends up holding
	// P1(enqueued 12:16), P2(enqueued 12:01) and T(settled 12:01:50). Replayed
	// anywhere in the ~15-minute band where now-SettledAt > 24h but
	// now-EnqueuedAt(P1) < 24h, T was discarded while P1 was admitted — and a
	// DELIVERED job went back into the pending set.
	//
	// So the discard is now conditional on the table: if the job is sitting
	// there PENDING, the tombstone is newer information than the pending record
	// and is APPLIED, however old it is. Applying it costs nothing — the entry
	// it produces is itself retired by the next sweep — and it is the only
	// ordering under which a settlement cannot be outlived by a stale sibling.
	if r.Retired(now, ob.settledRetention) && (!ok || existing.State.Terminal()) {
		// Not an error and not a loss: dropping it is the retention policy doing
		// its job, so the caller does not log it at ERROR. It is logged at Debug
		// so the path is not completely silent — and Debug is BELOW the default
		// level, so in a default deployment this particular discard is not
		// visible. That is deliberate rather than an oversight: invariant 6's
		// loud-discard rule is about losing a job, and nothing is lost here (the
		// job is already settled and its retention has simply run out).
		ob.log.Debug("discarding an outbox settlement record past its tombstone window; no pending job of that id remains to settle",
			"job_id", r.JobID, "state", r.State.String(), "source", source,
			"settled_at", r.SettledAt.UTC().Format(time.RFC3339Nano))
		return nil
	}

	if !ok {
		// GLOBAL PENDING CAPACITY IS ENFORCED ON EVERY PATH, INCLUDING REPLAY: a
		// memory bound one path could exceed is not a bound. A live Enqueue cannot
		// be refused here because it reserved its global slot before writing.
		//
		// The PER-PEER quota is deliberately NOT enforced here. It is a live
		// admission/fairness policy, not a recovery limit: builds predating that
		// policy could acknowledge up to MaxJobs pending records for one peer.
		// Discarding those durable records after an upgrade would turn a new
		// fairness setting into acknowledged message loss. Replay therefore admits
		// that legacy backlog up to the unchanged global memory bound; new live
		// enqueues for the peer remain refused until it drains below its quota.
		//
		// A TERMINAL RECORD DOES NOT CONSUME PENDING CAPACITY. Refusing a
		// tombstone for want of a slot is refusing the one record
		// that prevents a duplicate delivery: the settlement is dropped, and a
		// later pending record for that job resurrects it. Reachable in strict commit
		// order at default settings, because a job enqueued at T and settled at
		// T+23h58m has its PENDING record expire before its tombstone retires,
		// putting the tombstone on this insert path with no sibling to protect it.
		//
		// Tombstones are therefore time-bounded, not count-bounded. That is an
		// honest memory cost under high throughput, but imposing a count bound
		// would make delivery correctness depend on traffic rate. The outbox
		// stores routing metadata only, never message bodies, and every tombstone
		// retires after SettledRetention.
		//
		// Recovery never discards acknowledged state merely because a newer
		// binary configured a smaller capacity. put records the exact overage as
		// debt; Enqueue admits no growth until all applicable limits clear.
		ob.put(r.JobID, r)
		return nil
	}

	// A JOB'S IDENTITY NEVER CHANGES. The job id derives from these two fields,
	// so a mismatch is impossible for a validated record and can only come from
	// corruption or a splice — checked anyway, because "our own code wrote it"
	// is exactly the claim corruption disproves.
	//
	// # THE COMPLETE SET OF PLACES A TOMBSTONE CAN STILL BE REFUSED IS THREE
	//
	// Written out in full because an earlier version of this comment claimed
	// this check was "the one remaining guard" — and a comment that declares an
	// enumeration CLOSED while omitting a member tells the next reader to stop
	// looking, which is precisely how three separate resurrections (retention,
	// capacity, content) reached review in this file. Each of the three below is
	// safe for a DIFFERENT reason, and the reason is the part worth keeping:
	//
	//  1. THIS CHECK, the peer/origin mismatch. UNREACHABLE for validated
	//     records: outboxJobIDSep cannot occur in either half, so one job id
	//     recovers exactly one (peer, message) pair, and validate has already
	//     checked the stored id derives from the stored fields. If it did fire,
	//     the record would not describe THIS job at all, so applying it is the
	//     wrong answer — unlike retention, capacity and content, where the
	//     record genuinely was this job's settlement.
	//  2. THE SELF-ADDRESSED CHECK above. It refuses a terminal record too, but
	//     SYMMETRICALLY: the job id derives from PeerBusID, so every pending
	//     record for that job id is refused by the identical predicate. Nothing
	//     is left pending for the tombstone to have protected.
	//  3. Apply's DECODE FAILURE, upstream of this function entirely. It is the
	//     one that cannot be closed by an exemption — an undecodable record
	//     cannot be applied — and it is ASYMMETRIC: a settlement that fails to
	//     decode while its pending sibling decodes leaves the job pending. That
	//     is discard-and-log per invariant 6 rather than a defect, and the WAL's
	//     keyed MAC is what makes it not remotely reachable.
	if existing.PeerBusID != r.PeerBusID || existing.OriginMessageID != r.OriginMessageID {
		return fmt.Errorf("%w: the record describes a different (peer, message) pair for job %s", ErrInvalidOutboxRecord, r.JobID)
	}
	// SAME JOB, DIFFERENT CONTENT. A message id is minted by the origin bus and
	// never reused (invariant 1), so two different bodies under one id is either
	// corruption or a substitution attempt. The FIRST content wins.
	//
	// A SETTLEMENT IS NEVER REFUSED BY THIS CHECK, AND THAT EXCEPTION IS THE
	// POINT. Refusing a terminal record here left a DELIVERED job sitting
	// PENDING — a settlement losing to a pending record, which is the one
	// direction this file forbids, and the same defect class as the retention
	// and capacity holes above wearing a third predicate. Both gates found it
	// independently. So a mismatch between two PENDING records is still refused
	// and the first content kept, but a settlement is APPLIED to whatever the
	// table already holds: the job stops being owed, the first content stands,
	// and the contradiction is logged loudly rather than resolved in the
	// direction that sends a message twice.
	if existing.ContentSHA256 != r.ContentSHA256 || existing.Size != r.Size {
		if r.State.Terminal() && existing.State == OutboxPending {
			settled := existing
			settled.State = r.State
			settled.SettledAt = r.SettledAt
			settled.Reason = r.Reason
			// Clamped for the same reason Settle clamps: the entry is only ever
			// held in memory here, but a settled_at behind the enqueue it is
			// attached to would be a record this package would refuse to write.
			clamped := false
			if settled.SettledAt.Before(settled.EnqueuedAt) {
				settled.SettledAt = settled.EnqueuedAt
				clamped = true
			}
			ob.put(r.JobID, settled)
			// origin_message_id and reason are carried HERE because this
			// branch returns before the switch, so the ABANDONED warning below
			// never fires for a merged record. Without them a message this bus
			// will never deliver would be recorded only as a content
			// contradiction — not silent, but not SPECIFIC, which is what
			// invariant 6 actually asks for.
			ob.log.Error("an outbox SETTLEMENT names different content from the pending record it settles; the job is SETTLED anyway and the first content is kept, because refusing the settlement would leave a delivered message queued to be sent again",
				"job_id", r.JobID, "peer_bus", r.PeerBusID, "source", source,
				"origin_message_id", r.OriginMessageID, "state", r.State.String(), "reason", r.Reason,
				"kept_content", existing.ContentSHA256, "kept_size", existing.Size,
				"settlement_content", r.ContentSHA256, "settlement_size", r.Size,
				// Reported because the stored settled_at can otherwise differ
				// from the one in the durable record with nothing on the line
				// explaining the gap — Settle's own clamp logs a Warn, and an
				// operator reconciling the table against the WAL needs the same
				// signal here.
				"settled_at_clamped", clamped,
				"settlement_settled_at", r.SettledAt.UTC().Format(time.RFC3339Nano),
				"stored_settled_at", settled.SettledAt.UTC().Format(time.RFC3339Nano))
			return nil
		}
		return fmt.Errorf("%w: job %s already names content %s (%d bytes); a record claiming different content for the same message id is refused",
			ErrInvalidOutboxRecord, r.JobID, existing.ContentSHA256, existing.Size)
	}

	switch {
	case existing.State == OutboxPending && r.State == OutboxPending:
		// A re-applied enqueue: the same log replayed twice, or a live fold
		// Apply already performed. Idempotent when it is genuinely the same
		// record; refused when two DIFFERENT pending records claim one job,
		// because keeping the earlier EnqueuedAt is what stops the age horizon
		// being pushed out indefinitely by re-enqueueing.
		if !existing.EnqueuedAt.Equal(r.EnqueuedAt) {
			// THE ONE ALREADY APPLIED IS KEPT — which under commit-order replay
			// is the earlier one, but the rule is "first applied", not "earliest
			// enqueued", and saying otherwise would misdescribe what the code
			// does. Enqueue's inflight reservation is what actually stops two
			// pending records for one job from being written.
			return fmt.Errorf("%w: two different pending records claim job %s (holding the one enqueued %s, refusing the one enqueued %s); the record already applied is kept, so the age horizon cannot be pushed out by re-queueing",
				ErrInvalidOutboxRecord, r.JobID,
				existing.EnqueuedAt.UTC().Format(time.RFC3339Nano), r.EnqueuedAt.UTC().Format(time.RFC3339Nano))
		}
		return nil

	case existing.State == OutboxPending:
		// THE SETTLE: pending -> delivered or pending -> abandoned. The only
		// transition this table performs.
		ob.put(r.JobID, r)
		if r.State == OutboxAbandoned {
			// A message this bus will never deliver. Invariant 6: the discard is
			// recorded specifically, never silently.
			ob.log.Warn("an outbox job was ABANDONED; this message will never reach the peer",
				"job_id", r.JobID, "peer_bus", r.PeerBusID, "origin_message_id", r.OriginMessageID,
				"reason", r.Reason, "source", source)
		}
		return nil

	case r.State == OutboxPending:
		// THE RESURRECTION. A pending record for a job the durable history says
		// is settled: a stale record replayed out of order, a duplicate from a
		// spliced log, or an enqueue racing a settle. Refusing it is the single
		// most important line in this file — accepting it would re-send a
		// message the peer has already taken, and at-least-once would silently
		// become at-least-twice.
		return fmt.Errorf("%w: refusing a NON-MONOTONIC transition %s -> %s for job %s; a settled job is never resurrected, because re-sending a delivered message is a duplicate the peer may not deduplicate once the applied key has aged out",
			ErrOutboxSettled, existing.State, r.State, r.JobID)

	case existing.State == r.State:
		if existing.SettledAt.Equal(r.SettledAt) && existing.Reason == r.Reason {
			// The same settlement, re-applied. Idempotent.
			return nil
		}
		// Two settlements of the same KIND but not the same event. Benign for
		// correctness (the job stays settled either way), so it is a Warn and
		// the first is kept.
		ob.log.Warn("a redundant outbox settlement record; the first settlement is kept",
			"job_id", r.JobID, "state", r.State.String(), "source", source,
			"kept", existing.SettledAt.UTC().Format(time.RFC3339Nano),
			"discarded", r.SettledAt.UTC().Format(time.RFC3339Nano))
		return nil

	default:
		// delivered <-> abandoned. Contradictory: one of them is wrong about
		// whether the peer got the message. The first is kept, and this is
		// loud, because it means two code paths settled one job.
		return fmt.Errorf("%w: job %s is already %s; a contradicting %s record is refused and the first is kept",
			ErrOutboxSettled, r.JobID, existing.State, r.State)
	}
}

// sweepLocked drops records that have outlived their window: pending jobs past
// the retry horizon and settled tombstones past the retention window.
//
// EVERY DROPPED PENDING JOB IS LOGGED AT WARN, individually and by name. It is a
// message this bus accepted responsibility for and will now never deliver, which
// is precisely the discard invariant 6 says must never be silent. A settled
// tombstone ageing out is routine and is logged at Debug.
//
// # THE SWEEP WRITES NO DURABLE TOMBSTONE, AND THAT IS A REQUIREMENT ON RELAY-19
//
// A horizon drop removes the record from memory and logs it; it does NOT write
// an "abandoned" record. It cannot: sweepLocked runs with mu held, and this
// package never holds the lock across a durable write. The consequence is real
// and must not be discovered later — after a drop, the same job id is
// Enqueue-able again with a FRESH enqueue anchor, which is exactly the horizon
// extension the pending-vs-pending rule refuses elsewhere.
//
// It is not a hole in part 1, because nothing but a forwarder decides to
// re-queue and no forwarder reaches this code yet. It IS a requirement on
// RELAY-19: a forwarder must not re-enqueue a job it has seen dropped past the
// horizon, and if a durable abandonment is wanted it has to be written by a
// caller OUTSIDE the lock, as Settle already does.
//
// The caller must hold mu.
func (ob *Outbox) sweepLocked(now time.Time) {
	_, checkpointed := ob.durable.(outboxCheckpointer)
	for id, r := range ob.jobs {
		if _, already := ob.expired[id]; already {
			continue
		}
		switch {
		case r.FutureDated(now, OutboxClockSkewAllowance):
			// The live-table half of the admission guard. Without it a record
			// admitted under a FAST clock stays pending forever once the clock
			// is corrected backwards: Expired's subtraction is negative, so the
			// horizon never fires and the forwarder would retry past
			// idem.PeerOutageBudget, where the peer has forgotten the applied
			// key and takes the retry as a NEW message (invariant 10).
			if checkpointed {
				ob.expired[id] = struct{}{}
			} else {
				ob.del(id)
			}
			ob.log.Warn("DROPPING an outbox job stamped further ahead of the clock than the skew allowance; the clock moved backwards under it, and a future-dated job can never age out. This message will never reach the peer",
				"job_id", r.JobID, "peer_bus", r.PeerBusID, "origin_message_id", r.OriginMessageID,
				"enqueued_at", r.EnqueuedAt.UTC().Format(time.RFC3339Nano),
				"clock", now.UTC().Format(time.RFC3339Nano),
				"allowance", OutboxClockSkewAllowance.String())
		case r.Expired(now, ob.retryHorizon):
			if checkpointed {
				ob.expired[id] = struct{}{}
			} else {
				ob.del(id)
			}
			ob.log.Warn("DROPPING an outbox job that has passed the retry horizon; this message will never reach the peer",
				"job_id", r.JobID, "peer_bus", r.PeerBusID, "origin_message_id", r.OriginMessageID,
				"enqueued_at", r.EnqueuedAt.UTC().Format(time.RFC3339Nano),
				"age", now.Sub(r.EnqueuedAt).Truncate(time.Second).String(),
				"horizon", ob.retryHorizon.String())
		case r.Retired(now, ob.settledRetention):
			if checkpointed {
				ob.expired[id] = struct{}{}
			} else {
				ob.del(id)
			}
			ob.log.Debug("retiring a settled outbox record past its tombstone window",
				"job_id", r.JobID, "state", r.State.String(),
				"settled_at", r.SettledAt.UTC().Format(time.RFC3339Nano))
		}
	}
}

// Name, Kinds, Snapshot and Restore implement wal.CheckpointParticipant.
func (ob *Outbox) Name() string    { return "relay-outbox" }
func (ob *Outbox) Kinds() []string { return []string{OutboxRecordKind} }

const outboxCheckpointVersion = 1

type outboxCheckpointSnapshot struct {
	Version   int               `json:"version"`
	HighWater uint64            `json:"high_water"`
	Records   []json.RawMessage `json:"records"`
}

type outboxCheckpointCandidate struct {
	omittedBodies  map[string][]byte
	terminalBodies map[string][]byte
}

func (ob *Outbox) Snapshot(highWater uint64) ([]byte, error) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.sweepLocked(ob.now())
	ids := make([]string, 0, len(ob.jobs))
	for id := range ob.jobs {
		if _, omit := ob.expired[id]; !omit {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	s := outboxCheckpointSnapshot{Version: outboxCheckpointVersion, HighWater: highWater, Records: make([]json.RawMessage, 0, len(ids))}
	candidate := &outboxCheckpointCandidate{
		omittedBodies:  make(map[string][]byte, len(ob.expired)),
		terminalBodies: make(map[string][]byte),
	}
	for id := range ob.expired {
		body, err := ob.jobs[id].Encode()
		if err != nil {
			return nil, fmt.Errorf("relay: snapshot omitted outbox job %s: %w", id, err)
		}
		candidate.omittedBodies[id] = append([]byte(nil), body...)
	}
	for _, id := range ids {
		body, err := ob.jobs[id].Encode()
		if err != nil {
			return nil, fmt.Errorf("relay: snapshot outbox job %s: %w", id, err)
		}
		s.Records = append(s.Records, body)
		if ob.jobs[id].State.Terminal() {
			candidate.terminalBodies[id] = append([]byte(nil), body...)
		}
	}
	body, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	ob.checkpointCandidate = candidate
	return body, nil
}

func (ob *Outbox) Restore(snapshot []byte, highWater uint64) error {
	var s outboxCheckpointSnapshot
	dec := json.NewDecoder(bytes.NewReader(snapshot))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return fmt.Errorf("relay: decode outbox checkpoint: %w", err)
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("relay: outbox checkpoint has trailing JSON values")
	}
	if s.Version != outboxCheckpointVersion || s.HighWater != highWater {
		return fmt.Errorf("relay: outbox checkpoint version/high-water mismatch: version=%d high_water=%d supplied=%d", s.Version, s.HighWater, highWater)
	}
	records := make([]OutboxRecord, 0, len(s.Records))
	seen := make(map[string]struct{}, len(s.Records))
	last := ""
	for i, raw := range s.Records {
		r, err := DecodeOutboxRecord(raw)
		if err != nil {
			return fmt.Errorf("relay: decode outbox checkpoint record %d: %w", i, err)
		}
		canonical, err := r.Encode()
		if err != nil || !bytes.Equal(canonical, raw) {
			return fmt.Errorf("relay: outbox checkpoint record %d is not canonical", i)
		}
		if _, duplicate := seen[r.JobID]; duplicate {
			return fmt.Errorf("relay: duplicate outbox checkpoint job %s", r.JobID)
		}
		if last != "" && r.JobID < last {
			return fmt.Errorf("relay: outbox checkpoint records are not JobID-sorted")
		}
		seen[r.JobID] = struct{}{}
		last = r.JobID
		records = append(records, r)
	}
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if len(ob.inflight) != 0 || ob.pendingWrites != 0 {
		return errors.New("relay: cannot restore outbox checkpoint into an active outbox")
	}
	ob.jobs = make(map[string]OutboxRecord, len(records))
	ob.pendingJobs = 0
	ob.pendingByPeer = make(map[string]int)
	ob.retainedJobs = 0
	ob.retainedBytes = 0
	ob.retainedByPeer = make(map[string]int)
	ob.reservationBytes = make(map[string]int)
	ob.expired = make(map[string]struct{})
	for _, r := range records {
		ob.put(r.JobID, r)
	}
	ob.restoredHighWater = highWater
	ob.logCapacityDebtLocked("restore")
	return nil
}

// Checkpoint publishes the snapshot and only then reclaims records omitted for
// retention. Any failure, including a poisoned/ambiguous WAL handoff, leaves
// the serving table and all reservations intact.
func (ob *Outbox) Checkpoint() error {
	cp, ok := ob.durable.(outboxCheckpointer)
	if !ok {
		return errors.New("relay: outbox durable log does not support checkpoints")
	}
	ob.checkpointMu.Lock()
	defer ob.checkpointMu.Unlock()
	ob.mu.Lock()
	ob.checkpointCandidate = nil
	ob.mu.Unlock()
	if err := cp.Checkpoint(); err != nil {
		ob.mu.Lock()
		ob.checkpointCandidate = nil
		ob.mu.Unlock()
		return err
	}
	ob.mu.Lock()
	defer ob.mu.Unlock()
	candidate := ob.checkpointCandidate
	ob.checkpointCandidate = nil
	if candidate == nil {
		return errors.New("relay: checkpoint publication completed without an outbox snapshot candidate")
	}
	for id, omittedBody := range candidate.omittedBodies {
		r, ok := ob.jobs[id]
		_, stillExpired := ob.expired[id]
		if !ok {
			continue
		}
		body, err := r.Encode()
		if err == nil && stillExpired && bytes.Equal(body, omittedBody) {
			ob.del(id)
			continue
		}
		// A lifecycle transition committed into the new WAL tail while the
		// checkpoint was publishing. The snapshot omitted an older incarnation,
		// so cleanup must retain this one for parity with tail replay.
		delete(ob.expired, id)
		ob.rebaseTerminalLocked(id, r, body, err)
	}
	// The published snapshot makes each surviving terminal record the complete
	// durable lifecycle fact, so its earlier worst-case settle reservation can
	// now be rebased to the exact canonical bytes. Never do this before success:
	// an older generation may still recover the pending record and require the
	// full reserved settlement budget in its tail.
	for id, publishedBody := range candidate.terminalBodies {
		r, ok := ob.jobs[id]
		if !ok || !r.State.Terminal() {
			continue
		}
		body, err := r.Encode()
		if err != nil || !bytes.Equal(body, publishedBody) {
			continue // validated serving records make this unreachable
		}
		ob.rebaseTerminalLocked(id, r, body, err)
	}
	return nil
}

func (ob *Outbox) rebaseTerminalLocked(id string, r OutboxRecord, body []byte, err error) {
	if err != nil || !r.State.Terminal() {
		return
	}
	old := ob.reservationBytes[id]
	ob.reservationBytes[id] = len(body)
	ob.retainedBytes += len(body) - old
}

func (ob *Outbox) logCapacityDebtLocked(source string) {
	if ob.pendingJobs <= ob.maxJobs && ob.retainedJobs <= ob.maxRetainedJobs && ob.retainedBytes <= ob.maxRetainedBytes {
		debt := false
		for _, n := range ob.retainedByPeer {
			if n > ob.maxRetainedPerPeer {
				debt = true
				break
			}
		}
		if !debt {
			return
		}
	}
	ob.log.Warn("outbox recovered retained-capacity debt; acknowledged state is kept and new growth is blocked until a successful retention checkpoint drains it",
		"source", source, "pending_jobs", ob.pendingJobs, "max_pending_jobs", ob.maxJobs,
		"retained_jobs", ob.retainedJobs, "max_retained_jobs", ob.maxRetainedJobs,
		"retained_bytes", ob.retainedBytes, "max_retained_bytes", ob.maxRetainedBytes)
}

func outboxLifecycleReservation(r OutboxRecord) (int, error) {
	pending, err := r.Encode()
	if err != nil {
		return 0, err
	}
	max := len(pending)
	for _, state := range []OutboxState{OutboxDelivered, OutboxAbandoned} {
		t := r
		t.State = state
		t.SettledAt = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
		if t.SettledAt.Before(t.EnqueuedAt) {
			t.SettledAt = t.EnqueuedAt
		}
		if state == OutboxAbandoned {
			t.Reason = strings.Repeat("\x00", MaxOutboxReasonLen)
		} else {
			t.Reason = ""
		}
		body, e := t.Encode()
		if e != nil {
			return 0, e
		}
		if len(body) > max {
			max = len(body)
		}
	}
	return max, nil
}

// canonicalOutboxRecord round-trips a record through its own encoder so the
// value folded into memory is EXACTLY the value replay will decode.
//
// It is the same discipline invite.canonical applies, and it is not paranoia:
// without it a live fold could hold a time.Time with a monotonic clock reading
// or a sub-nanosecond component the RFC3339Nano round trip drops, and memory
// would then differ from disk in a way no test that never restarts can see.
func canonicalOutboxRecord(r OutboxRecord) (OutboxRecord, json.RawMessage, error) {
	body, err := r.Encode()
	if err != nil {
		return OutboxRecord{}, nil, err
	}
	canon, err := DecodeOutboxRecord(body)
	if err != nil {
		// Unreachable for a record Encode accepted; it is checked rather than
		// asserted because "unreachable" is a claim about code that changes.
		return OutboxRecord{}, nil, fmt.Errorf("relay: an outbox record this package encoded does not decode again: %w", err)
	}
	return canon, body, nil
}
