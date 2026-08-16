package ack

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// DurableLog is the two-phase write path this table records through.
//
// It is the same one-method seam relay.OutboxDurableLog and hub.DurableLog use,
// and it is an INTERFACE rather than a *wal.Log so this package can be tested
// against a log that kills the process mid-write (see ack_crash_test.go in
// internal/hub).
type DurableLog interface {
	Write(e wal.Entry) (wal.Committed, error)
}

// Options configures a Store.
type Options struct {
	// Logger receives every refused transition, every capacity refusal and
	// every discard. It may be nil.
	Logger *logging.Logger

	// Now supplies the clock every predicate here is evaluated against. nil
	// means time.Now.
	Now func() time.Time

	// MaxEntries is the hard cap on retained rows. 0 means MaxEntries. A test
	// may set it small: a bound that only exists at 65536 rows is a bound no
	// test can ever demonstrate.
	MaxEntries int

	// Retention overrides the retention window. 0 means Retention.
	Retention time.Duration
}

// Store is the durable, bounded, sender-visible delivery lifecycle table.
//
// See doc.go for the three properties that make the implementation safe, and
// state.go for why there is no `unknown` member and no hop-ACK method.
type Store struct {
	// mu guards every field below. A plain Mutex rather than an RWMutex: every
	// exported entry point sweeps first, and sweeping mutates.
	mu sync.Mutex

	log *logging.Logger
	now func() time.Time

	durable    DurableLog
	maxEntries int
	retention  time.Duration

	records map[key]Record

	// expiry is the retention queue: one entry per (row, anchor), ordered by
	// deadline, popped from the front. It is what makes sweepLocked O(expired)
	// rather than O(retained) — see sweepLocked for why that distinction is a
	// denial-of-service question here and not a style one.
	//
	// THE LIVE REGION IS expiry[expiryHead:], NOT expiry. Slots before the head
	// are already popped and have been ZEROED. Every reader must start at the
	// head; there are exactly three (sweepLocked, compactExpiryLocked, and
	// pushExpiryLocked's append), all in this file.
	expiry []expiryEntry

	// expiryHead is the index of the oldest un-popped entry in expiry. It is
	// what makes the sweep cost what it POPPED rather than what it RETAINED
	// (IDEM-19).
	//
	// # Why this is needed when the pop loop was already O(expired)
	//
	// The loop below was always O(expired) — it pops from the front and stops at
	// the first live entry. The cost was in what followed it: the whole
	// surviving tail was copied into a fresh backing array on every sweep that
	// popped even ONE entry, which made the sweep O(retained) and put the
	// package's own capitalised "IT IS O(EXPIRED), NOT O(RETAINED)" claim at
	// odds with its code.
	//
	// TestSweepIsNotOccupancyLinear did not catch it because it asserts
	// sweptEntries == 0 — it exercises only the case where NOTHING has expired,
	// where the loop breaks immediately and `drop == 0` returns before reaching
	// the copy. sweptEntries counts POPS, and the copy is not a pop. The
	// expensive shape is STAGGERED deadlines: pop a few, find the next live,
	// copy the rest, repeat. MEASURED on this queue in clean overlays of HEAD,
	// by BenchmarkAckSweepDrainStaggered: draining a staggered 65536-row table
	// took 51.00s before this change and 29.08ms after (1754x). The SETTLED
	// shape, where every row carries two queue entries, went 11.59s -> 7.62ms.
	//
	// That matters more here than in internal/idem because of WHERE it runs.
	// Every exported entry point sweeps, and one production Accept sweeps THREE
	// times (its own, Apply's during the live wal write, and foldIn's), all
	// inside Hub.publish with the GLOBAL WRITE LOCK held — so every writer on
	// the bus pays, not just the caller. And this queue holds up to two entries
	// per row — bounded at 2*maxEntries (131072) LIVE entries, with the dead
	// prefix below allowing an allocation of up to twice that — so one sweep
	// could copy twice what internal/idem's did.
	expiryHead int

	// bySender counts retained rows per SENDER — the principal charged for the
	// row, because the sender is who caused it to exist and who alone may read
	// it (§13.3).
	bySender map[string]int

	// writes and writesBySender are rows RESERVED across an fsync: admitted,
	// not yet folded in. They are counted in every bound so two concurrent
	// admissions cannot both pass a check that only one of them fits.
	writes         int
	writesBySender map[string]int

	// inflight reserves ONE pair across the fsync of a transition, and it is a
	// CORRECTNESS mechanism rather than an optimisation.
	//
	// Every mutating method decides under mu, releases mu, and only then writes
	// — it must, because wal.Log.Write calls Applier.Apply synchronously and
	// this Store IS that applier. Without a reservation, two callers offering
	// DIFFERENT terminal outcomes both read the same non-terminal row, both pass
	// the "is it already terminal" check, and both write. Memory then converges
	// correctly (upsertLocked keeps the first terminal) but BOTH callers are
	// told success, which §8.2 note 4 forbids — the second is a protocol
	// violation and must be REJECTED — and every future replay logs an ERROR
	// discard for that pair for the life of the record, on the exact line
	// invariant 6 reserves for a genuinely lost outcome.
	//
	// This is the same defect relay.Outbox.Enqueue's `inflight` map closes, with
	// the same remedy and for the same stated reason (outbox.go, "THE
	// RESERVATION IS TAKEN, NOT MERELY CHECKED").
	inflight map[key]struct{}

	// sweptEntries counts expiry-queue entries this store has POPPED, ever. It
	// is the work the sweep actually does, and it exists so
	// TestSweepIsNotOccupancyLinear can assert that work is independent of how
	// many rows are retained — a timing assertion would be flaky and would not
	// say what it means.
	sweptEntries uint64

	// capacityRefusals counts every row NOT created because a bound refused it.
	// It is the number that makes §11.3's degradation observable rather than
	// silent, and it is never reset.
	capacityRefusals uint64
	lastCapacityLog  time.Time
}

// NewStore builds an unattached table. Every mutating method refuses with
// ErrNotDurable until Attach.
//
// It takes NO durable log on purpose: it is a wal.Applier, so it must exist
// BEFORE wal.Open in order to be in the applier map that replay feeds — the
// same build-before-Open, Attach-after three-step the enrolment roster, the
// invite store and the relay outbox all use (cmd/agent-bus/main.go). Replay
// therefore always finishes before the first live write, structurally rather
// than by remembering to.
func NewStore(o Options) *Store {
	s := &Store{
		log:            o.Logger,
		now:            o.Now,
		maxEntries:     o.MaxEntries,
		retention:      o.Retention,
		records:        make(map[key]Record),
		bySender:       make(map[string]int),
		writesBySender: make(map[string]int),
		inflight:       make(map[key]struct{}),
	}
	if s.log == nil {
		// The same nil-logger convention relay.NewOutbox uses, rather than a nil
		// check at every call site: a discard logger cannot be the reason a
		// discard goes unlogged.
		s.log = logging.New(io.Discard, logging.LevelError)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.maxEntries <= 0 {
		s.maxEntries = MaxEntries
	}
	if s.retention <= 0 {
		s.retention = Retention
	}
	return s
}

// Attach binds the durable log, after wal.Open has replayed it.
func (s *Store) Attach(d DurableLog) error {
	if d == nil {
		return fmt.Errorf("ack: attaching the delivery lifecycle durable log: it must not be nil; a table with no log would report delivery outcomes that no restart could reproduce (%w)", ErrNotDurable)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.durable != nil {
		return errors.New("ack: attaching the delivery lifecycle durable log: already attached; one in-memory table must not have two durable histories")
	}
	s.durable = d
	return nil
}

// durableLog reads the attached log under the lock.
//
// A method rather than a bare field read because Attach WRITES s.durable after
// construction: an unsynchronised read from a mutating method would be a data
// race with it, and a race here is a P0 (concurrency is the product). Every
// caller captures the value ONCE and uses the captured local for both its nil
// check and its later Write, so it cannot check one log and write to another.
func (s *Store) durableLog() DurableLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.durable
}

// Name and Kinds make this a wal.Applier for the multiplex applier.
func (s *Store) Name() string    { return "ack-lifecycle" }
func (s *Store) Kinds() []string { return []string{RecordKind} }

// Apply folds one committed record into the serving copy.
//
// It is the REPLAY path and it NEVER returns an error, because an error from an
// applier whose COMMIT record is already durable poisons the whole log with
// wal.ErrDiverged. A record this table cannot use is DISCARDED — and every
// discard is logged at ERROR, loudly and specifically, naming the pair and the
// state, because invariant 6 rates the SILENT discard as the defect rather than
// the discard.
//
// It is also called for a LIVE write, since the multiplex applier dispatches
// this kind here; the mutating methods fold the identical canonical record in
// again afterwards, which is a no-op by upsertLocked's idempotency.
func (s *Store) Apply(c wal.Committed) error {
	if c.Entry.Kind != RecordKind {
		return nil
	}
	r, err := DecodeRecord(c.Entry.Body)
	if err != nil {
		s.log.Error("DISCARDING a delivery lifecycle record that could not be decoded; the sender-visible state for that message will read as `unknown` rather than as its real outcome",
			"prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex, "err", err)
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.upsertLocked(r, s.now(), "replay", true); err != nil {
		s.log.Error("DISCARDING a delivery lifecycle record that could not be applied; the pair keeps the state already in memory",
			"prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex,
			"correlation_key", r.CorrelationKey, "recipient", r.Recipient,
			"state", r.State.String(), "err", err)
	}
	return nil
}

// Accept records LOCAL ACCEPTANCE — event E1, the only transition this build
// writes from production code.
//
// It returns only once the row is on stable storage (invariant 4): the write is
// wal.Log.Write's Begin(fsync) -> Commit(fsync), and a nil error means the
// record is durable AND visible in memory.
//
// # THE CALLER MUST HAVE MADE THE MESSAGE DURABLE FIRST
//
// `accepted` asserts "this bus has committed and fsynced the message". Calling
// this before the message's own commit would make the row claim a durability the
// message does not have; internal/hub calls it after the message's two-phase
// write has returned.
//
// # A SECOND CALL FOR THE SAME PAIR IS A LEGITIMATE RETRY
//
// It returns the ORIGINAL result, writes nothing, re-applies nothing and does
// not error — invariant 10's first case. That holds whatever state the row has
// reached: a retry that arrives after the recipient already ACKed must not
// reopen a terminal row, and telling the caller its send "failed" because the
// row moved on would be a lie about an operation that demonstrably succeeded.
//
// # THE CAPACITY REFUSAL IS THE ONE PLACE THIS DESIGN DEGRADES RATHER THAN FAILS CLOSED
//
// A caller that sees ErrCapacity or ErrAgentQuota MUST STILL SUCCEED ITS SEND
// (§11.3). Everywhere else in this repository the fail-closed answer is to
// refuse the operation; here refusing would mean an OBSERVABILITY table causing
// a MESSAGING outage, and worse, it would break everything while violating
// nothing — the message is already durable and the sender was already told 201.
// Degrading the observation is recoverable; refusing the send is not. The loud
// log below is what stops that being silent.
func (s *Store) Accept(correlationKey, sender, recipient string) error {
	durable := s.durableLog()
	if durable == nil {
		return ErrNotDurable
	}
	now := s.now().UTC()
	rec := Record{
		CorrelationKey: correlationKey,
		Recipient:      recipient,
		Sender:         sender,
		State:          StateAccepted,
		AcceptedAt:     now,
	}
	// Canonicalised BEFORE the table is touched, so the record folded into
	// memory is byte-identical to the one replay will read back.
	canon, body, err := canonical(rec)
	if err != nil {
		return err
	}
	k := canon.key()

	s.mu.Lock()
	s.sweepLocked(now)
	if _, ok := s.records[k]; ok {
		s.mu.Unlock()
		// The legitimate retry. Nothing is written and the ORIGINAL AcceptedAt
		// stands, so the retention window fires from the first acceptance and
		// can never be pushed out by retrying. The row is deliberately NOT
		// inspected: a retry that arrives after the recipient already ACKed must
		// not reopen a terminal row, and telling the caller its send "failed"
		// because the row moved on would be a lie about an operation that
		// demonstrably succeeded.
		return nil
	}
	if err := s.admitLocked(canon, now); err != nil {
		s.mu.Unlock()
		return err
	}
	// The PAIR is reserved as well as the capacity slot, so two concurrent
	// acceptances of the same pair cannot both write. They would agree (the
	// record is identical bar its timestamp) and the second would be an
	// idempotent no-op rather than a discard, so this is the mild case — but
	// leaving it open would put a redundant record in an append-only log on
	// every raced retry, and one reservation covering all three mutating
	// methods is simpler to reason about than one that covers two of them.
	if err := s.reserveLocked(k); err != nil {
		s.mu.Unlock()
		return err
	}
	s.writes++
	s.writesBySender[canon.Sender]++
	s.mu.Unlock()
	defer s.release(k)

	// THE RELEASE IS DEFERRED SO IT RUNS AFTER foldIn, NOT BEFORE IT. Releasing
	// the reservation first would reopen the exact window it exists to close: a
	// concurrent Accept could take the freed slot between the decrement and the
	// fold, and this record — which is ALREADY DURABLE — would then be refused
	// by the in-memory bound. Same ordering as relay.Outbox.Enqueue and
	// invite.Store.Mint, for the same reason.
	defer s.releaseWriteSlot(canon.Sender)

	if _, err := durable.Write(wal.Entry{Kind: RecordKind, Body: body}); err != nil {
		// NOTHING was acknowledged and nothing is in memory.
		return fmt.Errorf("ack: writing the acceptance record for %s -> %s: %w", canon.CorrelationKey, canon.Recipient, err)
	}
	s.foldIn(canon, "accept")
	return nil
}

// Settle durably moves a pair to a TERMINAL state — events E4 (undeliverable),
// E5 (delivered) and E6 (refused).
//
// NOTHING IN THIS BUILD CALLS IT. ACK-4/ACK-5 own the bus-emitted terminals and
// ACK-6 owns the recipient-emitted ones; it exists and is tested here because a
// state machine that is only half-expressible cannot be reviewed as a state
// machine, and because the monotonicity rule it enforces is what makes replay
// order irrelevant.
//
// The three outcomes, never collapsed (invariant 10):
//
//   - No row for this pair -> ErrNoRecord. Nothing binds the claim (§8.2 note 1).
//     The caller answers §13.3's uniform answer, logs it, and DOES NOT DISCONNECT.
//   - The SAME terminal outcome already recorded -> nil, nothing written. A
//     legitimate retry: return the original result, re-apply nothing.
//   - A DIFFERENT terminal outcome -> ErrTerminal. A protocol violation: reject
//     AND log, DO NOT DISCONNECT. The FIRST terminal stands.
//
// class must be "" for StateDelivered, a recipient-emitted class for
// StateRefused and a bus-emitted class for StateUndeliverable; validate refuses
// every other combination in both directions.
//
// # IT TAKES NO PRINCIPAL, AND THE CALLER THEREFORE OWES ONE
//
// READ THIS BEFORE WIRING A ROUTE TO IT. There is no authenticated-principal
// parameter, so this method DOES NOT and CANNOT check that the caller is
// entitled to settle this pair. It is safe today only because nothing calls it.
//
// The obligation is not hypothetical and it does not belong here: the two layers
// that authorise a settlement are §6.1 (which BUS spoke, via
// RequirePeerPrincipal) and §6.2 (the obligation binding —
// `DeriveJobID(peer, key)` must name an outbox job THIS bus durably wrote), and
// both live at the route. A recipient-side ACK is authorised differently again:
// the caller must be the RECIPIENT this row names. So:
//
//	ACK-4 / ACK-5 (peer surface)  MUST apply §6.2's job binding before calling.
//	ACK-6 (agent surface)         MUST prove the authenticated principal EQUALS
//	                              `recipient` before calling.
//
// Without that, agent B can mark agent A's message `refused`. upsertLocked's
// sender guard does NOT cover it — this method copies the sender forward from
// the row it found, so that guard can never fire from this path.
func (s *Store) Settle(correlationKey, recipient string, state State, class Class, attestedBy Attestation) error {
	durable := s.durableLog()
	if durable == nil {
		return ErrNotDurable
	}
	if !state.Terminal() {
		return fmt.Errorf("%w: %s is not a terminal state; a settle moves a row OUT of the non-terminal states and terminal is absorbing", ErrInvalidRecord, state)
	}
	// VALIDATED BEFORE ANYTHING IS LOOKED UP OR LOGGED. The caller of this
	// method is a ROUTE (ACK-4, ACK-6), so both strings are remote input, and
	// invariant 1 makes a client-supplied correlation key input to be validated
	// rather than an identity to be trusted. Validating after the lookup would
	// echo a hostile megabyte verbatim into an error string and an operator log
	// line, which is the exact expansion elide() exists to prevent.
	k, err := validatePair(correlationKey, recipient)
	if err != nil {
		return err
	}
	now := s.now().UTC()

	s.mu.Lock()
	s.sweepLocked(now)
	existing, ok := s.records[k]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s -> %s", ErrNoRecord, k.correlationKey, k.recipient)
	}
	if existing.State.Terminal() {
		s.mu.Unlock()
		if existing.State == state && existing.Class == class && existing.AttestedBy == attestedBy {
			// Byte-identical outcome: a legitimate retry, absorbed silently.
			return nil
		}
		s.log.Error("REFUSING a second, DIFFERENT terminal delivery outcome; the FIRST terminal stands and nothing is written. This is invariant 10's protocol-violation case: it is rejected and logged, and it does NOT disconnect anything",
			"correlation_key", k.correlationKey, "recipient", k.recipient,
			"recorded_state", existing.State.String(), "recorded_class", string(existing.Class),
			"offered_state", state.String(), "offered_class", string(class))
		return fmt.Errorf("%w: %s -> %s is already %s and %s was offered", ErrTerminal, k.correlationKey, k.recipient, existing.State, state)
	}
	// THE PAIR IS RESERVED ACROSS THE FSYNC, and this is what makes the check
	// above binding rather than advisory: without it two callers offering
	// DIFFERENT terminals both read this non-terminal row, both pass, and both
	// are told success. See Store.inflight.
	if err := s.reserveLocked(k); err != nil {
		s.mu.Unlock()
		return err
	}
	// AcceptedAt is carried forward from the row that already exists, never
	// re-stamped: a terminal row records when the message was ACCEPTED, not when
	// it settled, and re-stamping would push the retention anchor out.
	next := Record{
		CorrelationKey: existing.CorrelationKey,
		Recipient:      existing.Recipient,
		Sender:         existing.Sender,
		State:          state,
		Class:          class,
		AttestedBy:     attestedBy,
		AcceptedAt:     existing.AcceptedAt,
		SettledAt:      now,
	}
	s.mu.Unlock()
	// DEFERRED so it runs after foldIn, never before it: releasing first would
	// let a concurrent transition take the freed slot between the release and
	// the fold, and decide against a row this one has already made durable.
	defer s.release(k)

	canon, body, err := canonical(next)
	if err != nil {
		return err
	}
	// NO capacity reservation: this row already exists and a transition does not
	// grow the table. Charging it again would let a full table strand live rows
	// in a non-terminal state for ever — the opposite of the degradation §11.3
	// chooses, since a terminal outcome that cannot be recorded is a REAL result
	// lost, not merely an unobserved one.
	if _, err := durable.Write(wal.Entry{Kind: RecordKind, Body: body}); err != nil {
		return fmt.Errorf("ack: writing the %s record for %s -> %s: %w", state, canon.CorrelationKey, canon.Recipient, err)
	}
	s.foldIn(canon, "settle")
	return nil
}

// MarkInFlight records event E2: at least one hop is owed for this pair,
// because a pending outbox job exists. Remote recipients only — a local
// recipient never reaches it (§8.3).
//
// NOTHING IN THIS BUILD CALLS IT; ACK-5 does.
//
// # THIS IS NOT A HOP ACK, AND THERE IS NO METHOD THAT IS
//
// E2 is "a hop is OWED". A hop ACK (E3, relay.RelayResponse.Accepted) is "a peer
// took responsibility for a copy" and it MOVES THE SENDER-VISIBLE STATE NOT AT
// ALL — it changes the HOP record (relay.OutboxDelivered, which already exists)
// and nothing else. That is the entire point of the epic (§8.2 note 3, §8.4).
// The absence of a method here is the enforcement.
//
// A row that is already in_flight, or already terminal, is left alone: a second
// destination does not change the state, and terminal is absorbing.
func (s *Store) MarkInFlight(correlationKey, recipient string) error {
	durable := s.durableLog()
	if durable == nil {
		return ErrNotDurable
	}
	// Validated before anything is looked up or logged, for the reason Settle
	// states: this method's caller is a route, so both strings are remote input.
	k, err := validatePair(correlationKey, recipient)
	if err != nil {
		return err
	}
	now := s.now().UTC()

	s.mu.Lock()
	s.sweepLocked(now)
	existing, ok := s.records[k]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s -> %s", ErrNoRecord, k.correlationKey, k.recipient)
	}
	if existing.State == StateInFlight {
		s.mu.Unlock()
		return nil
	}
	if existing.State.Terminal() {
		s.mu.Unlock()
		// COUNTED AND LOGGED, NOT AN ERROR. A hop being enqueued after the
		// recipient already ACKed is NORMAL under at-least-once delivery, so
		// this is absorption rather than a fault.
		s.log.Debug("IGNORING an in-flight transition on a terminal delivery lifecycle row; terminal is absorbing",
			"correlation_key", k.correlationKey, "recipient", k.recipient, "state", existing.State.String())
		return nil
	}
	// Reserved across the fsync for the reason Settle states: without it this
	// method can write a stale in_flight record AFTER a concurrent Settle's
	// terminal one, and every future replay then logs an ERROR discard for a
	// record that was never wrong.
	if err := s.reserveLocked(k); err != nil {
		s.mu.Unlock()
		return err
	}
	next := existing
	next.State = StateInFlight
	s.mu.Unlock()
	defer s.release(k)

	canon, body, err := canonical(next)
	if err != nil {
		return err
	}
	if _, err := durable.Write(wal.Entry{Kind: RecordKind, Body: body}); err != nil {
		return fmt.Errorf("ack: writing the in-flight record for %s -> %s: %w", canon.CorrelationKey, canon.Recipient, err)
	}
	s.foldIn(canon, "in-flight")
	return nil
}

// Lookup returns the retained row for a pair.
//
// A false second result means NOT RETAINED — swept, never created, or never
// accepted — and a caller reporting to a sender must render that as `unknown`
// (§8.1). It must NOT be rendered as any durable state, and `unknown` must never
// come back here to be written.
//
// # IT IS A POINT LOOKUP, AND ACK-9 MUST NOT BUILD ITS ROUTE OUT OF A LOOP OVER IT
//
// §13.1's route answers ONE ROW PER RECIPIENT for a correlation key, and §13.3
// rules that only the ORIGINAL SENDER may read a row — every other case, key
// never existed, key swept, key belongs to someone else, gets the SAME answer,
// `unknown`. A handler that iterated candidate recipients through this method
// would satisfy the letter of that and break it completely: the loop IS the
// oracle, because the caller chooses the recipients it probes and learns which
// ones exist.
//
// So ACK-9 owes a `rows for (correlation key) filtered by sender == principal`
// accessor, and the filter belongs INSIDE it rather than at the handler. It is
// deliberately not added here: it needs its own secondary index and its own
// share of the memory bound, and adding an unused index now would be a
// derivation nobody could check.
//
// This method returns the whole Record, INCLUDING Sender, which is what makes
// that filter implementable at all — but returning it is not permission to serve
// it: Sender is authorisation input, never output.
func (s *Store) Lookup(correlationKey, recipient string) (Record, bool) {
	// VALIDATED, and the refusal is INDISTINGUISHABLE from a miss. Both mutating
	// methods validate, and a read that did not would be the laxer of the two —
	// which is backwards, since the read is the one a route exposes to an
	// unauthenticated-shaped probe.
	//
	// It returns (Record{}, false) rather than an error ON PURPOSE: false means
	// NOT RETAINED, a caller renders that as `unknown`, and §13.3 requires the
	// SAME answer for a key that never existed, a key that was swept and a key
	// that is somebody else's. A distinct "malformed" answer would be a fourth
	// case, and a caller that reported it would tell a prober which of its
	// guesses were even well-formed. Closing that in code rather than in a
	// comment is the point.
	k, err := validatePair(correlationKey, recipient)
	if err != nil {
		return Record{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
	r, ok := s.records[k]
	return r, ok
}

// Len reports retained rows, after a sweep.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
	return len(s.records)
}

// Stats is what makes the bound and the degradation OBSERVABLE rather than
// assumed: an operator needs to see the table filling BEFORE it refuses, and
// needs to see how many observations were dropped once it did.
//
// # OPERATOR-ONLY. Senders IS A ROSTER-SIZE ORACLE AND MUST NOT REACH AN AGENT
//
// Senders counts DISTINCT PRINCIPALS holding at least one row, which is a lower
// bound on bus membership. §5.5 refuses aggregate delivery counts for exactly
// this reason — "an aggregate is a roster-size oracle: it discloses bus
// membership to any sender" — and the reasoning transfers unchanged. This struct
// is for an operator inspect/metrics surface, and no field of it may be served
// on the agent surface. It is documented rather than removed because the number
// is genuinely needed to see the fair share engage.
type Stats struct {
	Entries          int
	MaxEntries       int
	Senders          int
	UnderPressure    bool
	CapacityRefusals uint64
}

// Stats reports the table's occupancy after a sweep.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
	return Stats{
		Entries:          len(s.records),
		MaxEntries:       s.maxEntries,
		Senders:          len(s.bySender),
		UnderPressure:    len(s.records)+s.writes >= s.pressureLineLocked(),
		CapacityRefusals: s.capacityRefusals,
	}
}

// pressureLineLocked derives the fair-share trigger from the STORE'S OWN
// maxEntries, never from the constant — a bound that only exists at 65536 rows
// is a bound no test can ever demonstrate.
func (s *Store) pressureLineLocked() int { return s.maxEntries / 2 }

// admitLocked is the capacity gate for a NEW row. The caller holds mu.
//
// # IT EVICTS NOTHING, AND THAT IS THE DECISION
//
// Evicting a live row turns a real terminal outcome into a false `unknown` — an
// INVERSION of the truth, not a gap in it — and it would do it quietly, to the
// row most likely to matter. Refusal is loud and recoverable; inversion is
// neither. Identical posture to idem.ErrCapacity, and it must read as the same
// decision rather than a different one.
//
// # THE PER-SENDER FAIR SHARE, AND WHY THE DIVISOR IS senders+1
//
//	under pressure : entries >= maxEntries / 2
//	fair share     : maxEntries / (senders + 1)
//
// The "+1" is THE SENDER THAT HAS NOT ARRIVED YET, and it is load-bearing rather
// than a safety fudge: with a divisor of `senders`, a lone sender's share is the
// WHOLE table, so one authenticated agent filling everything before any victim
// holds a single row passes straight through and the rule buys nothing. The
// victim cannot be counted, because it holds nothing PRECISELY BECAUSE it is
// being starved.
func (s *Store) admitLocked(r Record, now time.Time) error {
	held := len(s.records) + s.writes
	if held >= s.maxEntries {
		s.refuseLocked(r, now, "the table is at its hard entry cap", held, s.maxEntries)
		return fmt.Errorf("%w: %d rows are retained or being written against a limit of %d; the delivery lifecycle row for %s -> %s is NOT created and nothing is evicted to make room. THE MESSAGE IS UNAFFECTED",
			ErrCapacity, held, s.maxEntries, r.CorrelationKey, r.Recipient)
	}
	if held < s.pressureLineLocked() {
		return nil
	}
	share := s.maxEntries / (len(s.bySender) + 1)
	senderHeld := s.bySender[r.Sender] + s.writesBySender[r.Sender]
	if senderHeld >= share {
		s.refuseLocked(r, now, "this sender is at its fair share", senderHeld, share)
		return fmt.Errorf("%w: sender %s holds %d rows against its fair share of %d while the table is under pressure (%d of %d); the delivery lifecycle row for %s -> %s is NOT created. THE MESSAGE IS UNAFFECTED",
			ErrAgentQuota, r.Sender, senderHeld, share, held, s.maxEntries, r.CorrelationKey, r.Recipient)
	}
	return nil
}

// refuseLocked counts and logs a refused row. The caller holds mu.
//
// The FIRST refusal after a quiet period is always logged at ERROR with the
// remedy; the repetitions are throttled to one per capacityLogInterval and carry
// the running total, so a busy bus cannot flood its own log and push the
// informative line out of retention. See capacityLogInterval.
func (s *Store) refuseLocked(r Record, now time.Time, why string, held, limit int) {
	s.capacityRefusals++
	if !s.lastCapacityLog.IsZero() && now.Sub(s.lastCapacityLog) < capacityLogInterval {
		return
	}
	s.lastCapacityLog = now
	s.log.Error("DELIVERY STATUS IS BEING DROPPED: "+why+", so no sender-visible lifecycle row was created for this message and GET /v1/ack will report `unknown` for it. THE MESSAGE ITSELF IS DURABLE AND WAS ACCEPTED (201) — this degrades the OBSERVATION, never the send, because an observability table must not cause a messaging outage. Nothing is evicted to make room: evicting a live row would turn a real outcome into a false `unknown`, which is worse than a gap",
		"correlation_key", r.CorrelationKey,
		"recipient", r.Recipient,
		"sender", r.Sender,
		"held", held,
		"limit", limit,
		"rows_refused_total", s.capacityRefusals,
		"retention", s.retention.String(),
		"remedy", "rows age out after the retention window; a bus sustaining more than maxEntries/retention new (message, recipient) pairs per second will sit at this cap continuously",
	)
}

// validatePair bounds and parses the two remote-supplied strings that identify a
// row, returning the composite key.
//
// It exists so the rule has ONE implementation rather than one per method: a
// client-supplied correlation key is INPUT TO BE VALIDATED, never an identity to
// be trusted (invariant 1), and a recipient must be fully qualified
// (invariant 2). Skipping it hands unbounded caller-chosen bytes to a map lookup
// and to whatever is logged about the miss.
func validatePair(correlationKey, recipient string) (key, error) {
	if err := validateMessageID("correlation_key", correlationKey); err != nil {
		return key{}, err
	}
	if err := validateAgentID("recipient", recipient); err != nil {
		return key{}, err
	}
	return key{correlationKey: correlationKey, recipient: recipient}, nil
}

// reserveLocked claims a pair for the duration of one durable write. The caller
// holds mu and MUST arrange for release to run after its foldIn.
func (s *Store) reserveLocked(k key) error {
	if _, busy := s.inflight[k]; busy {
		return fmt.Errorf("%w: %s -> %s", ErrConcurrentTransition, k.correlationKey, k.recipient)
	}
	s.inflight[k] = struct{}{}
	return nil
}

func (s *Store) release(k key) {
	s.mu.Lock()
	delete(s.inflight, k)
	s.mu.Unlock()
}

func (s *Store) releaseWriteSlot(sender string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes--
	s.writesBySender[sender]--
	if s.writesBySender[sender] <= 0 {
		delete(s.writesBySender, sender)
	}
}

// foldIn applies a record produced by a LIVE, already-durable operation.
//
// A refusal is logged at ERROR and SWALLOWED: the record is on stable storage,
// so the durable log is the truth and a restart rebuilds memory from it.
// Returning an error would tell the caller an operation failed that demonstrably
// did not.
func (s *Store) foldIn(r Record, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.upsertLocked(r, s.now(), source, false); err != nil {
		s.log.Error("a delivery lifecycle record is DURABLE but was refused by the in-memory table; the durable log is the truth and a restart will rebuild from it",
			"correlation_key", r.CorrelationKey, "recipient", r.Recipient,
			"state", r.State.String(), "source", source, "err", err)
	}
}

// upsertLocked is the ONE place anything enters or changes in the table, and it
// is MONOTONIC.
//
// # WHAT MONOTONICITY IS KEYED ON, AND WHY REPLAY ORDER IS THEREFORE SAFE
//
// It is keyed on the STATE RANK OF THE PAIR — accepted below in_flight below
// terminal — and on nothing else. Not on a sequence number, not on a timestamp,
// not on the record's position in the log. That choice is what makes the rule
// survive a log replayed with holes in it:
//
//   - TERMINAL IS ABSORBING. delivered/refused/undeliverable never becomes
//     accepted or in_flight again, whatever order the records arrive in. A stale
//     `accepted` record replayed AFTER a `delivered` one is REFUSED — otherwise
//     a settled outcome is resurrected as an open one and a sender is told a
//     delivered message is still in flight.
//   - THE FIRST TERMINAL RECORD WINS. Two contradicting terminals cannot both be
//     true; keeping the first keeps the outcome that actually happened, and the
//     second is refused and logged.
//   - A DUPLICATE IS A NO-OP. The same record applied twice — the log replayed
//     twice, or a live fold that Apply already performed — is idempotent,
//     because the transition it asks for has already happened.
//
// # WHAT THIS DOES *NOT* CLAIM, STATED BECAUSE THE WIDER CLAIM IS THE DANGEROUS ONE
//
// THE CAPACITY CAP IS ORDER-SENSITIVE. Which records get in depends on which
// arrived while there was room, so a terminal record discarded by the cap
// followed by that pair's `accepted` record would reopen it. That is unreachable
// under commit-order replay — a pair's acceptance always precedes its settlement
// in a well-formed log, so the tombstone is never the one discarded — but it is
// the reason the cap must never be relied on as a second monotonicity mechanism.
//
// RETENTION IS THE OTHER ORDER DEPENDENCY. The sweep runs first, so a terminal
// row swept before a stale `accepted` record for the same pair arrives lets the
// pair reappear as accepted. Under commit-order replay the two are minutes apart
// at most and the window is 24h, so it cannot occur; it is written down rather
// than left as an unexamined "any order".
//
// SO THE GUARANTEE IS EXACTLY PER-PAIR COMMIT ORDER, WHICH IS WHAT wal PROVIDES.
//
// enforceCap is true only on the replay path. A live fold has ALREADY reserved
// its slot across the fsync (Accept), so re-charging it there would let the
// table refuse a record that is on stable storage.
//
// The caller must hold mu.
func (s *Store) upsertLocked(r Record, now time.Time, source string, enforceCap bool) error {
	s.sweepLocked(now)

	k := r.key()
	existing, ok := s.records[k]
	if !ok {
		if enforceCap && len(s.records) >= s.maxEntries {
			return fmt.Errorf("%w: %d rows retained against a limit of %d; this record is DISCARDED and nothing is evicted",
				ErrCapacity, len(s.records), s.maxEntries)
		}
		s.putLocked(k, r)
		return nil
	}
	// A pair's identity includes its sender: the sender is the principal charged
	// for the row and the only party authorised to read it, so a record claiming
	// a different sender for a pair this bus already holds is not a transition,
	// it is a different claim about the same message.
	if existing.Sender != r.Sender {
		return fmt.Errorf("%w: %s -> %s is held for sender %s and the record names %s; a message has one sender",
			ErrInvalidRecord, r.CorrelationKey, r.Recipient, existing.Sender, elide(r.Sender))
	}
	if existing.State == r.State && existing.Class == r.Class && existing.AttestedBy == r.AttestedBy {
		// Idempotent re-apply. The ORIGINAL record stands, including its
		// timestamps: taking the later record's SettledAt would let a replay
		// push a terminal row's retention anchor forward every time it ran.
		return nil
	}
	if existing.State.Terminal() {
		return fmt.Errorf("%w: %s -> %s is %s (settled at %s); %s was offered and the first terminal stands",
			ErrTerminal, r.CorrelationKey, r.Recipient, existing.State,
			existing.SettledAt.UTC().Format(time.RFC3339Nano), r.State)
	}
	if r.State.rank() <= existing.State.rank() {
		return fmt.Errorf("%w: %s -> %s is %s and %s would move it backwards; the lifecycle advances only",
			ErrInvalidRecord, r.CorrelationKey, r.Recipient, existing.State, r.State)
	}
	// AcceptedAt is preserved from the row already held, not taken from the
	// incoming record. They agree on every path this package writes (Settle and
	// MarkInFlight both carry it forward), and preferring the held value means a
	// record that disagreed could not extend the retention window.
	next := r
	next.AcceptedAt = existing.AcceptedAt
	s.putLocked(k, next)
	return nil
}

// expiryEntry is one queued retention deadline. See Store.expiry.
type expiryEntry struct {
	k        key
	deadline time.Time
}

// putLocked is the ONE place a row enters or is replaced, and the ONE place the
// expiry queue grows. Keeping both here is what stops a row existing that the
// sweep can never reach.
func (s *Store) putLocked(k key, r Record) {
	prev, existed := s.records[k]
	if !existed {
		s.bySender[r.Sender]++
	}
	s.records[k] = r
	// Queued on INSERT, and again only when the ANCHOR MOVES — which happens
	// exactly once per row, when it settles (§11 measures a non-terminal row
	// from accepted_at and a terminal one from settled_at). accepted ->
	// in_flight keeps the same anchor and must NOT re-queue, or a row could be
	// pushed once per transition and the queue would stop being bounded by the
	// table.
	if !existed || (r.State.Terminal() && !prev.State.Terminal()) {
		s.pushExpiryLocked(r)
	}
}

func (s *Store) delLocked(k key) {
	r, ok := s.records[k]
	if !ok {
		return
	}
	delete(s.records, k)
	s.bySender[r.Sender]--
	if s.bySender[r.Sender] <= 0 {
		delete(s.bySender, r.Sender)
	}
}

// sweepLocked retires rows past the retention window (§11, event E9).
//
// A swept row reports as `unknown`, which is the honest answer: the window is
// chosen so nothing can still change after it, so the bus genuinely no longer
// knows.
//
// # IT IS O(EXPIRED), NOT O(RETAINED), AND THAT IS A CORRECTNESS PROPERTY HERE
//
// TWO mechanisms are required for that claim, and for a while this comment named
// only the first — which made it a claim the code did not deliver:
//
//  1. It pops expired entries off the FRONT of an ordered queue and STOPS at the
//     first live one, rather than ranging the map (internal/idem's expireLocked,
//     which §11.2 says to mirror exactly).
//  2. It COMPACTS THE QUEUE ONLY WHEN THE DEAD PREFIX HAS GROWN TO THE SIZE OF
//     THE LIVE SUFFIX (compactExpiryLocked). Without this, every sweep that
//     popped even one entry copied the whole surviving tail, and the sweep was
//     O(retained) no matter how good the pop loop was.
//
// Point 2 was missing until IDEM-19, and the gap is instructive rather than
// embarrassing: TestSweepIsNotOccupancyLinear was written to guard exactly this
// property and PASSED throughout, because it asserts sweptEntries == 0 and so
// only ever exercises the case where NOTHING expired — where the loop breaks at
// once and `drop == 0` returns before the copy is reached. sweptEntries counts
// POPS; the copy is not a pop. Draining a staggered 65536-row table measured
// 51.00s before the fix and 29.08ms after (BenchmarkAckSweepDrainStaggered, in
// clean overlays of HEAD). The machine-independent part is the CURVE:
// quadrupling the retained set multiplied the drain by ~15-17x before and ~4x
// after.
//
// So: do not restore an unconditional compaction here, and do not judge this
// property by reading the pop loop alone.
//
// An earlier revision ranged the whole map instead. That is not a style
// difference, because of WHERE this runs: every exported entry point sweeps, and
// Accept sweeps three times (its own, Apply's during the live wal write, and
// foldIn's), all of it inside Hub.publish with the GLOBAL WRITE LOCK held. A
// full-map scan therefore made every send on the bus pay for the table's
// occupancy — measured at 13.9 ms per Accept at the 65536-row cap, against
// 107 µs at 1024, with no fsync in the measurement at all. One authenticated
// agent reaching its own fair share (32768 rows) taxed every OTHER agent's sends
// for the full 24h window, at zero marginal cost to itself, and added ~10
// minutes to startup replay. A bound that is cheap to reach and expensive for
// everybody else is a denial-of-service mechanism, not a bound.
//
// # WHAT THE QUEUE HOLDS, AND WHY A STALE ENTRY IS HARMLESS
//
// One entry per (row, anchor). A row is pushed when it is inserted (deadline
// AcceptedAt+retention) and pushed AGAIN when it settles, because settling moves
// the anchor to SettledAt (§11). Anchors only ever move LATER, so a row has at
// most two entries and the queue stays bounded at 2*maxEntries LIVE entries.
//
// That bound is on LIVE entries, not on the allocation: since IDEM-19 the slice
// also carries a dead prefix of popped, zeroed slots, bounded strictly below the
// live suffix (expiryHead, compactExpiryLocked), so len(expiry) can be up to
// twice the live count. The distinction is stated here because this is the doc a
// reader reaches first, and an unqualified "bounded at 2*maxEntries" would be a
// bound the code no longer holds to.
//
// Nothing is ever removed from the middle — that would be the O(n) this exists
// to avoid. A popped entry is therefore only a HINT that a row MIGHT be
// expired, and the row's own Expired predicate decides. Three cases, all
// handled: the row is gone (drop the entry), the row was re-anchored and is
// still live (drop the entry; its later one is behind), or the row really is
// expired (delete it).
//
// # WHAT A BACKWARDS CLOCK STEP DOES, STATED RATHER THAN ASSUMED
//
// Deadlines are appended in operation order, so they are non-decreasing only
// while the clock is. A backwards step can leave one deadline out of order,
// after which the "stop at the first live entry" rule holds a few rows LONGER
// than the window. That is the SAFE direction and is deliberately chosen: an
// over-retained row still reports its true outcome, while an under-retained one
// reports `unknown` about something the bus does know — an inversion of the
// truth rather than a gap in it, which is the same trade §11.2 makes by evicting
// nothing.
//
// The caller must hold mu.
func (s *Store) sweepLocked(now time.Time) {
	start := s.expiryHead
	for s.expiryHead < len(s.expiry) {
		e := s.expiry[s.expiryHead]
		if now.Before(e.deadline) {
			// The queue is ordered, so the first live entry proves every entry
			// behind it is live too.
			break
		}
		s.sweptEntries++
		if r, ok := s.records[e.k]; ok {
			if r.Expired(now, s.retention) {
				s.delLocked(e.k)
			}
			// else: re-anchored by a settle, and the later entry is behind this
			// one. Dropping this stale entry is the whole point of the hint.
		}
		s.expiryHead++
	}
	if s.expiryHead == start {
		return
	}
	// ZERO EVERY SLOT JUST VACATED, before deciding whether to compact.
	//
	// This is the half of the old "compact into a fresh array" that was load
	// bearing, and it is kept in full. Advancing an index alone would leave the
	// backing array still referencing every popped entry — and an expiryEntry
	// holds a key, which is two strings — so the memory the sweep was supposed
	// to release would not be released: the same retain-forever trap
	// internal/idem's expireLocked calls out. Zeroing drops those references
	// NOW, on the same O(popped) pass, whenever the array is next compacted.
	for i := start; i < s.expiryHead; i++ {
		s.expiry[i] = expiryEntry{}
	}
	s.compactExpiryLocked()
}

// compactExpiryLocked reclaims the dead prefix of the expiry queue, but only
// once it has grown to at least the size of the live suffix. The caller holds
// mu.
//
// # Why the copy is CONDITIONAL, when it used to be unconditional
//
// Copying the survivors on every sweep is what made this O(retained) despite the
// front-popped queue, and it cost 51.00s to drain a staggered 65536-row table
// against 29.08ms for this form. Deferring until head >= live means each copy
// moves at most as many entries as there have been POPS since the previous copy
// — the head resets to 0 here, so the prefix must be rebuilt from scratch before
// another copy is due — which is amortised O(1) per popped entry.
//
// # The two bounds that keep "defer the copy" from meaning "leak"
//
//   - Wasted CAPACITY is bounded: the dead prefix is never allowed to exceed the
//     live suffix, so the queue's length stays under twice its live size, and
//     the live size is itself bounded at 2*maxEntries by §11's one-entry-per-
//     (row, anchor) rule. The documented 2*maxEntries bound is therefore a bound
//     on LIVE entries; the allocation may be up to twice that transiently, which
//     is stated here rather than left for a reader to discover.
//   - Wasted MEMORY is zero regardless: sweepLocked zeroes each vacated slot as
//     it goes, so a dead slot holds no key and pins no string.
//
// A fully-drained queue releases its array outright rather than keeping a
// peak-sized allocation alive on a bus that has gone quiet — up to 2*maxEntries
// live entries, and up to twice that in slots once the dead prefix is counted.
func (s *Store) compactExpiryLocked() {
	live := len(s.expiry) - s.expiryHead
	if live == 0 {
		s.expiry = nil
		s.expiryHead = 0
		return
	}
	if s.expiryHead < live {
		return
	}
	// A FRESH backing array, exactly as the unconditional form used: the old one
	// is dropped whole, so nothing about which memory is reachable changes —
	// only how often this runs.
	kept := make([]expiryEntry, live)
	copy(kept, s.expiry[s.expiryHead:])
	s.expiry = kept
	s.expiryHead = 0
}

// pushExpiryLocked queues r's current retention deadline. The caller holds mu.
func (s *Store) pushExpiryLocked(r Record) {
	anchor := r.AcceptedAt
	if r.State.Terminal() {
		anchor = r.SettledAt
	}
	if anchor.IsZero() {
		// Unreachable for a validated record. Queued at `now` rather than
		// skipped: an unqueued row is a row the sweep can never reach, which is
		// a leak with a bound written on it.
		anchor = s.now()
	}
	s.expiry = append(s.expiry, expiryEntry{k: r.key(), deadline: anchor.Add(s.retention)})
}

// ValidateCorrelationKey is the check a future status API must apply to a
// CLIENT-SUPPLIED key before it is used to look anything up.
//
// It is exported so a route that reads a key out of a URL path can apply the
// same rule the mutating methods apply internally, from one implementation
// rather than one per route: a client-supplied correlation key is INPUT TO BE
// VALIDATED, never an identity to be trusted (invariant 1). A caller that skips
// it hands unbounded caller-chosen bytes to a map lookup and to whatever it logs
// about the miss.
func ValidateCorrelationKey(v string) error { return validateMessageID("correlation_key", v) }
