package hub

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// MaxIdempotencyKeyLen bounds a client-supplied idempotency key. It is the same
// bound and the same alphabet internal/auth applies to the enrolment key
// (auth.MaxIdempotencyKeyLen), for the same reason: the key is reflected into
// the server log, so anything outside a short run of safe bytes is REJECTED
// rather than escaped and kept.
//
// It is restated here rather than imported because keeping one number in two
// packages is cheaper than the dependency, and because the two limits are free
// to diverge — they bound different surfaces.
const MaxIdempotencyKeyLen = 128

// MaxIdempotencyEntries bounds the applied-key table.
//
// It is a MEMORY bound, not the retention policy. Since IDEM-11 both live in
// internal/idem: the retention policy is idem.RetentionWindow, DERIVED term by
// term from the longest interval a client or peer could still be retrying over
// (see internal/idem/retention.go), and this bound is idem.MaxEntries, derived
// from an explicit per-record memory budget.
//
// It is DEFINED AS idem.MaxEntries rather than restated, so the number
// CONTRACTS-HTTP.md documents (65536) cannot move in one place only.
//
// The table FAILS CLOSED at this bound rather than evicting. Evicting a
// remembered key silently turns the next retry of it into a SECOND message,
// which is the double-apply invariant 10 forbids; a refused send is
// recoverable, a duplicated one is not. It is the same posture as
// auth.recordIdempotent.
const MaxIdempotencyEntries = idem.MaxEntries

// Batch-size bounds for the read paths.
const (
	// DefaultBatchLimit is the batch size when a client does not ask.
	DefaultBatchLimit = 64

	// MaxBatchLimit is the ceiling on a client-requested batch size. It bounds
	// the response an authenticated caller can ask this server to materialise
	// in one request; store.MaxBodyBytes times this is the worst case, so 256
	// caps one response at 16 MiB of body.
	MaxBatchLimit = 256
)

// Long-poll bounds.
const (
	// DefaultPollTimeout is how long a long-poll parks when the caller does not
	// ask and the server was configured with no ceiling of its own.
	DefaultPollTimeout = 30 * time.Second

	// MaxPollTimeout is the hard ceiling on a parked request, whatever the
	// client asks for and whatever the server is configured with. A parked
	// request holds a connection and a goroutine; unbounded parking is an
	// authenticated caller pinning server resources indefinitely.
	MaxPollTimeout = 5 * time.Minute

	// MaxWaitersPerAgent bounds how many long polls ONE agent may have parked
	// at once. See the check in Wait for why the bound is per-agent, why an
	// agent-id key is safe on this authenticated route, and why the real cost
	// it bounds is notify's scan rather than memory.
	//
	// 32 matches auth.DefaultMaxActiveSessionsPerAgent, and for the same
	// reason: a well-behaved agent needs one or two, and the slack is for an
	// agent driven from several processes or one that reconnects before its
	// old poll has drained.
	MaxWaitersPerAgent = 32
)

// DurableLog is the hub's view of the two-phase durable write path
// (internal/wal). *wal.Log satisfies it.
//
// One method, deliberately: Write is the whole of invariant 4 as this package
// needs it — hand over an entry, get back a Committed only once it is prepared,
// committed and fsynced. Begin/Commit/Abort/Close belong to the process
// lifecycle that main owns, and a hub that could reach them could reorder the
// one guarantee it exists to preserve.
type DurableLog interface {
	Write(wal.Entry) (wal.Committed, error)
}

// ReplayFunc streams every committed entry in the durable log, in commit order,
// to fn.
//
// It is INJECTED rather than called directly so this package does not have to
// open the WAL itself — main already opened it, and a second wal.Open on the
// same directory would be a second writer on the same file. The natural
// implementation is a closure over wal.Replay(path, fn), which is a read-only
// pass.
//
// The seam is also the migration path: once main passes the Hub as
// wal.LogOptions.Applier, replay happens once, inside wal.Open, and this hook
// is set to nil. (*Hub).Apply is already the right shape for that — it
// implements wal.Applier.
type ReplayFunc func(fn func(wal.Committed) error) (wal.Recovered, error)

// Options configures Open.
type Options struct {
	// BusID is this bus's server-minted id. REQUIRED: every message id and
	// every agent id is qualified with it (invariants 1 and 2).
	BusID string

	// Durable is the two-phase write path. When nil the hub serves reads and
	// refuses every send with ErrNotDurable — invariant 4 has no "best effort"
	// setting.
	Durable DurableLog

	// Replay rebuilds the serving copy from the durable log at startup. It may
	// be nil, in which case the hub starts with an EMPTY store: correct only
	// for a fresh bus or a test, and the caller is asserting that there is no
	// durable history to rebuild.
	Replay ReplayFunc

	// NextIndex is the durable log's high-water mark at open —
	// wal.Recovered.NextIndex, the index the next append will use. It is how
	// the sequence floor is derived; read the long comment in Open before
	// changing where this comes from.
	NextIndex uint64

	// Quarantined is the path the previous durable log was MOVED to when
	// recovery could not read it (wal.Repair.Quarantined), or "" for the
	// ordinary case. It is carried here ONLY so Open can say so at ERROR: a
	// quarantine starts a fresh log whose index restarts near 1, so the
	// sequence high-water mark from before it is unrecoverable and message ids
	// may repeat values the quarantined file already used.
	Quarantined string

	// Logger receives recovery, discard and poisoning events. Defaults to a
	// discarding logger.
	Logger *logging.Logger

	// Now is the clock. Defaults to time.Now.
	Now func() time.Time

	// PollTimeout is the default long-poll ceiling; 0 means
	// DefaultPollTimeout. It is clamped to MaxPollTimeout.
	PollTimeout time.Duration

	// MaxAge and MaxBytes are the message retention bounds; 0 means the
	// store package defaults (1 day / 1 GiB).
	MaxAge   time.Duration
	MaxBytes int64
}

// Hub owns message fan-out, the applied-key table and the long-poll waiter
// registry. It is safe for concurrent use.
//
// The zero value is not usable; construct with Open.
type Hub struct {
	busID       string
	durable     DurableLog
	log         *logging.Logger
	now         func() time.Time
	pollTimeout time.Duration

	seq   *ids.Sequence
	store *store.Store

	// writeMu serialises the WHOLE of publish: the applied-key check, the
	// sequence allocation, the durable write, the apply to memory and the wake.
	//
	// # Why it is this wide, and why that is not a performance bug
	//
	// The durable log already serialises transactions one at a time behind its
	// own lock (see wal.Log.mu), so a narrower lock here would buy no
	// concurrency on the fsync — it would only let two callers interleave the
	// steps AROUND it, which is precisely where the ordering guarantees live:
	// a sequence must not be allocated by one caller while another is between
	// its own allocation and its write, or the sequence order and the commit
	// order come apart and replay can no longer rebuild the store in order.
	//
	// # Lock order
	//
	// writeMu -> waitMu (the wake) and writeMu -> the store's own lock.
	//
	// rosterMu is deliberately NOT in that chain: publish validates the sender
	// and the recipients BEFORE it takes writeMu, so the two are never held
	// together at all. That is the strongest position available and it is worth
	// keeping — but it is also why Enrolled's TOCTOU note matters, so read that
	// before moving the roster check inside the lock to "tidy this up".
	//
	// Nothing takes writeMu while holding waitMu, rosterMu or the store lock,
	// so there is no cycle. The read paths (History, Wait, Agents) never take
	// writeMu at all, which is what keeps a long poll off the fsync path.
	writeMu sync.Mutex

	// idem is the durable applied-key table (IDEM-11). It is keyed on the
	// idem.Scope tuple — (agent, operation, key) — so one agent can neither
	// collide with nor probe for another's keys, and one agent cannot collide
	// with ITSELF across two routes.
	//
	// Every mutating call takes writeMu around it, but the Store has its own
	// lock too, because IdempotencyStats is read off writeMu by CORE-5's
	// inspect endpoint.
	idem *idem.Store

	// idemCapWarned records that the replay-time capacity warning has already
	// been emitted, so a large log produces ONE line rather than one per
	// message. Written only during Open, before the hub is reachable.
	idemCapWarned bool

	// poisoned, once set, is never cleared. See ErrPoisoned. Guarded by
	// writeMu.
	poisoned error

	// appliedSeq is the highest sequence seen by Apply during recovery. It is
	// written only during Open, before anything can reach the hub, and read
	// only there.
	appliedSeq uint64

	rosterMu sync.RWMutex
	roster   map[string]Agent

	// recovered holds every agent id named as a sender or a recipient by a
	// message replayed from disk at startup. It is written only during Open and
	// read-only afterwards, so it needs no lock.
	//
	// It exists to make ONE thing observable: an id that appears here and is
	// then enrolled again in this process is an id being REUSED, which
	// invariant 1 forbids. See noteIdentityReuse.
	recovered map[string]struct{}

	waitMu sync.Mutex
	// waiters is every parked long poll; waitersByAgent is the same set counted
	// per agent, for the per-agent cap. They are updated in the same critical
	// section and must never drift.
	waiters        map[*waiter]struct{}
	waitersByAgent map[string]int
}

// Result is what an accepted send or broadcast produced.
type Result struct {
	// MessageID is the server-minted "<bus-id>-<seq>" (invariant 1).
	MessageID string

	// Seq is the server-minted sequence number: the message's position in the
	// bus's total order.
	Seq uint64

	// Sender is the authenticated sender, fully qualified (invariant 2).
	Sender string

	// Broadcast reports which of the two send shapes this was.
	Broadcast bool

	// Recipients is the directed recipient list; empty for a broadcast.
	Recipients []string

	// SentAt is when this bus accepted the message.
	SentAt time.Time

	// Replayed reports that this result came from the applied-key table rather
	// than from a fresh send — the client retried and NOTHING was re-applied.
	//
	// It is not part of the message: it describes THIS call, so it is false in
	// the stored record and set only on the returned copy. The HTTP layer
	// surfaces it as a header, leaving the body identical between the original
	// and the replay.
	Replayed bool
}

// Open builds a Hub and rebuilds its serving copy from the durable log.
func Open(o Options) (*Hub, error) {
	if err := ids.ValidateBusID(o.BusID); err != nil {
		return nil, fmt.Errorf("hub: open: %w", err)
	}
	h := &Hub{
		busID:          o.BusID,
		durable:        o.Durable,
		log:            o.Logger,
		now:            o.Now,
		pollTimeout:    o.PollTimeout,
		roster:         make(map[string]Agent),
		recovered:      make(map[string]struct{}),
		waiters:        make(map[*waiter]struct{}),
		waitersByAgent: make(map[string]int),
	}
	if h.log == nil {
		h.log = logging.New(io.Discard, logging.LevelError)
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.pollTimeout <= 0 {
		h.pollTimeout = DefaultPollTimeout
	}
	if h.pollTimeout > MaxPollTimeout {
		h.pollTimeout = MaxPollTimeout
	}
	h.store = store.New(store.Options{MaxAge: o.MaxAge, MaxBytes: o.MaxBytes, Now: h.now})
	// Built after h.now is normalised so the applied-key table and the message
	// store read the SAME clock: two clocks would let a test (or a future
	// injected clock) expire messages and keys against different "now"s.
	h.idem = idem.NewStore(idem.StoreOptions{Now: h.now})

	// # Deriving the sequence floor — read this before changing it
	//
	// invariant 1: a sequence number is NEVER reused, including across
	// restarts. ids.Resume wants "the highest sequence EVER WRITTEN TO DISK —
	// prepared or committed, delivered or discarded", and it cannot check the
	// claim, so the claim has to be argued here.
	//
	// The floor used is the durable log's own high-water index, which
	// wal.Recovered documents as "strictly greater than EVERY index in the
	// file, INCLUDING indices burned by prepares that were discarded". The
	// argument that this bounds every sequence rests on one property that
	// publish maintains and CHECKS:
	//
	//	  every sequence this hub issues is <= the WAL index of the PREPARE
	//	  record that carried it.
	//
	// It holds by counting. At start the floor is NextIndex-1, so the first
	// sequence issued is NextIndex, which is exactly the index the next append
	// takes. Thereafter each published message consumes ONE sequence and at
	// least TWO indices (a prepare and a commit), so the indices outrun the
	// sequences by at least one per message and the gap only widens. Anything
	// else that ever writes to this log — AUTH-3's durable enrolment, DUR-5 —
	// consumes indices without consuming sequences and widens it further.
	//
	// Therefore every sequence on disk sits at or below an index on disk, so
	// the next start's NextIndex is strictly above every sequence ever written,
	// and Resume(NextIndex-1) is a sound floor. publish asserts the property
	// per message rather than trusting the counting argument to survive future
	// edits — see the check there and ErrPoisoned.
	//
	// The floor is deliberately NOT derived from the recovered store's highest
	// sequence. That is the obvious wiring and it is WRONG: it misses the
	// sequence of a DANGLING prepare (a message prepared but never committed,
	// the ordinary signature of a crash mid-write), which never reaches Apply,
	// and reissuing it is exactly what invariant 1's 2026-08-02 restatement
	// forbids — "recovery may not reissue an index it has already handed out,
	// even for a record it discards".
	floor := o.NextIndex
	if floor > 0 {
		floor--
	}
	h.seq = ids.Resume(floor)

	if o.Quarantined != "" {
		// LOUD, and not a nicety. A quarantine starts a FRESH log, so NextIndex
		// restarts near 1 while the quarantined file holds sequences far above
		// it — this hub can then reissue message ids the quarantined log
		// already used, which invariant 1 forbids. Nothing here can recover the
		// old high-water mark (the file was unreadable, which is why it was
		// quarantined), so the honest action is to say so at ERROR and carry on
		// serving, per invariant 6's availability-over-retention decision:
		// silent discard is the defect, discard is not.
		h.log.Error("the durable log was QUARANTINED, so the message sequence high-water mark from before the quarantine is UNKNOWN: message ids may repeat values the quarantined log already used (invariant 1). The quarantined file is the only record of them",
			"quarantined_to", o.Quarantined,
			"resumed_floor", floor,
		)
	}

	if o.Replay != nil {
		rec, err := o.Replay(h.Apply)
		if err != nil {
			// Recovery could not complete. Memory is not a prefix of anything
			// and must not be served (invariant 5).
			return nil, fmt.Errorf("hub: rebuilding the message store from the durable log: %w", err)
		}
		if rec.NextIndex > 0 {
			if err := h.seq.RaiseFloor(rec.NextIndex - 1); err != nil {
				return nil, fmt.Errorf("hub: raising the sequence floor to the replayed high-water mark: %w", err)
			}
		}
		h.log.Info("message store rebuilt from the durable log",
			"messages", h.store.Head(),
			"applied_high_water", h.appliedSeq,
			"records_replayed", rec.Records,
			"next_index", rec.NextIndex,
		)
	}

	// Belt and braces: whatever the counting argument says, never resume at or
	// below a sequence a replayed record proves was written.
	if h.appliedSeq > 0 {
		if err := h.seq.RaiseFloor(h.appliedSeq); err != nil {
			return nil, fmt.Errorf("hub: raising the sequence floor to the highest replayed sequence: %w", err)
		}
	}
	// Floor assembly ends here. Next refuses to issue until it does.
	if err := h.seq.Seal(); err != nil {
		return nil, fmt.Errorf("hub: sealing the sequence floor: %w", err)
	}
	return h, nil
}

// BusID returns this bus's id.
func (h *Hub) BusID() string { return h.busID }

// Store exposes the serving copy for operators and tests. It is the same
// instance the hub serves from, not a copy.
func (h *Hub) Store() *store.Store { return h.store }

// Apply implements wal.Applier: it folds one committed entry into the serving
// copy. It runs during recovery, before anything can reach this hub.
//
// # A record it cannot understand is DISCARDED, LOUDLY
//
// Returning an error here would abort recovery and refuse to start the bus.
// Invariant 6 settled that trade on 2026-08-02: recovery ALWAYS reaches a
// running server, damaged records are discarded, and the absolute requirement
// is that every discard is LOGGED, specifically — silent discard is the defect,
// not discard itself. So a message record that does not decode is reported at
// ERROR with its WAL index and skipped, and the bus starts.
//
// Entries of any other Kind are skipped SILENTLY and without complaint. That is
// not the same thing: AUTH-3 will write enrolment records into this same log,
// and a hub that treated them as damage would fill the log with false alarms.
func (h *Hub) Apply(c wal.Committed) error {
	if c.Entry.Kind != store.RecordKind {
		return nil
	}
	m, err := store.Decode(c.Entry.Body)
	if err != nil {
		h.log.Error("DISCARDING a message record that could not be decoded during recovery; it is not in this bus's history and will not be delivered",
			"prepare_index", c.PrepareIndex,
			"commit_index", c.CommitIndex,
			"err", err,
		)
		return nil
	}
	if err := h.store.Append(m); err != nil {
		h.log.Error("DISCARDING a message record that could not be applied during recovery; it is not in this bus's history and will not be delivered",
			"prepare_index", c.PrepareIndex,
			"message_id", m.ID,
			"seq", m.Seq,
			"err", err,
		)
		return nil
	}
	if m.Seq > h.appliedSeq {
		h.appliedSeq = m.Seq
	}
	// Every agent id this record names is now an id that has been WRITTEN TO
	// DISK, which is the fact Open's warning is built on.
	h.recovered[m.Sender] = struct{}{}
	for _, r := range m.Recipients {
		h.recovered[r] = struct{}{}
	}

	// The applied-key memory is REBUILT here, which is what makes it part of
	// recovered state rather than an in-memory cache (invariant 10). A client
	// that retries a send across a restart gets the original result, not a
	// second message.
	//
	// EXPIRY AND THE BOUND apply on this path exactly as on the live one, and
	// they are applied by the SAME code: idem.Store expires internally on every
	// call, from the record's own CommittedAt. That is what keeps memory and
	// disk from ever disagreeing about which keys are live (IDEM-11 point (f)):
	// eviction is a pure predicate re-derived on both paths, not a second
	// mechanism that could drift.
	rec, ok := h.recoverIdemRecord(c, m)
	if !ok {
		return nil
	}
	if err := h.idem.Remember(rec); err != nil {
		if errors.Is(err, idem.ErrCapacity) {
			// Not fatal, but the operator must know: the rebuilt table is a
			// PREFIX of the durable one, so keys beyond the cap will not
			// suppress a retry that the pre-restart bus would have suppressed.
			// Logged ONCE — one line per message would bury it.
			if !h.idemCapWarned {
				h.idemCapWarned = true
				h.log.Warn("the applied-key table reached its cap during recovery, so the rebuilt table is a PREFIX of the durable one: keys beyond the cap will not suppress a retry",
					"max_entries", MaxIdempotencyEntries,
					"prepare_index", c.PrepareIndex,
				)
			}
			return nil
		}
		h.log.Error("DISCARDING an applied-key record that could not be remembered during recovery; a retry of this key will be applied as a NEW operation",
			"prepare_index", c.PrepareIndex,
			"message_id", m.ID,
			"err", err,
		)
	}
	return nil
}

// recoverIdemRecord rebuilds the applied-key record for a replayed message,
// from the durable record when the entry carries one and from the message
// itself when it does not.
func (h *Hub) recoverIdemRecord(c wal.Committed, m store.Message) (idem.Record, bool) {
	if c.Entry.Idem != nil {
		rec, err := idem.DecodeRecord(c.Entry.Idem)
		if err != nil {
			// DISCARDED LOUDLY. Invariant 6 sanctions the discard; SILENT
			// discard is the defect. The message itself has already been
			// applied above, so this loses only the duplicate suppression for
			// that one key.
			h.log.Error("DISCARDING an applied-key record that could not be decoded during recovery; a retry of this key will be applied as a NEW operation",
				"prepare_index", c.PrepareIndex,
				"commit_index", c.CommitIndex,
				"message_id", m.ID,
				"err", err,
			)
			return idem.Record{}, false
		}
		return rec, true
	}

	// BACK-COMPAT, AND IT IS MANDATORY, NOT OPTIONAL.
	//
	// A log written BEFORE IDEM-11 carries no idem record in its prepare
	// payload, but it DOES carry the message's own idempotency key (it has been
	// a durable field of store.Record from the start, precisely so the
	// applied-key memory could be recovered state). Without this path, every
	// applied key in an existing on-disk log would be lost on the FIRST restart
	// after this change — turning a durability improvement into a durability
	// regression, and doing it exactly once, at the upgrade, where it is
	// hardest to notice.
	//
	// The fingerprint is recomputed with the SAME function publish uses, so a
	// record rebuilt this way is indistinguishable from one read off disk.
	if m.IdempotencyKey == "" {
		return idem.Record{}, false
	}
	op := idem.OpSend
	if m.Broadcast {
		op = idem.OpBroadcast
	}
	result, err := encodeStoredResult(m.ID, m.Seq, m.Recipients, m.SentAt)
	if err != nil {
		h.log.Error("DISCARDING an applied-key record rebuilt from a pre-IDEM-11 message record; a retry of this key will be applied as a NEW operation",
			"prepare_index", c.PrepareIndex,
			"message_id", m.ID,
			"err", err,
		)
		return idem.Record{}, false
	}
	return idem.Record{
		Agent:       m.Sender,
		Op:          op,
		Key:         m.IdempotencyKey,
		Fingerprint: publishFingerprint(op, m.Recipients, m.Body),
		Result:      result,
		Seq:         m.Seq,
		CommittedAt: m.SentAt,
	}, true
}

// BroadcastRequest is one broadcast attempt. Sender is the AUTHENTICATED
// principal and is supplied by the caller from the request context, never from
// the request body (invariant 1).
type BroadcastRequest struct {
	Sender         string
	Body           []byte
	IdempotencyKey string
}

// SendRequest is one directed send.
type SendRequest struct {
	Sender         string
	To             string
	Body           []byte
	IdempotencyKey string
}

// Broadcast durably records a message addressed to the whole bus and wakes
// every eligible waiter. It returns only once the message is committed and
// fsynced (invariant 4).
func (h *Hub) Broadcast(req BroadcastRequest) (Result, error) {
	return h.publish(req.Sender, true, nil, req.Body, req.IdempotencyKey)
}

// Send durably records a message addressed to one agent and wakes that agent's
// waiters. It returns only once the message is committed and fsynced
// (invariant 4).
//
// An unknown recipient is ErrUnknownRecipient and nothing is written.
func (h *Hub) Send(req SendRequest) (Result, error) {
	if _, _, _, err := ids.ParseAgentID(req.To); err != nil {
		return Result{}, fmt.Errorf("%w: %s", ErrInvalidRecipient, err)
	}
	return h.publish(req.Sender, false, []string{req.To}, req.Body, req.IdempotencyKey)
}

// publish is the ONE durable write path for a message. Broadcast and Send
// differ only in their addressing, so they must not differ in their durability,
// their idempotency or their wake-up — one function is how that is guaranteed
// rather than asserted.
//
// The ORDER below is the whole of invariants 4, 5 and 10 and must not be
// rearranged:
//
//  1. reject a retried key, or a key reused for different content
//  2. mint the sequence and the message id (server-authoritative, invariant 1)
//  3. WRITE THROUGH THE TWO-PHASE PATH AND FSYNC (invariant 4)
//  4. only then apply to the serving copy (invariant 5: disk is the truth)
//  5. only then remember the key and wake waiters (POLL-2)
//
// Step 5 after step 3 is the subtle one and is why POLL-2 exists as its own
// task: a waiter woken before the commit is durable can observe a message that
// a crash then un-observes, which is an acknowledged-but-lost message wearing a
// different hat.
func (h *Hub) publish(sender string, broadcast bool, recipients []string, body []byte, key string) (Result, error) {
	if err := validateIdempotencyKey(key); err != nil {
		return Result{}, err
	}
	if len(body) == 0 {
		return Result{}, fmt.Errorf("%w: a message body is required", ErrInvalidBody)
	}
	if len(body) > store.MaxBodyBytes {
		return Result{}, fmt.Errorf("%w: %d bytes, the limit is %d", ErrInvalidBody, len(body), store.MaxBodyBytes)
	}
	if !h.Enrolled(sender) {
		return Result{}, fmt.Errorf("%w: %q", ErrUnknownSender, sender)
	}
	for _, r := range recipients {
		if !h.Enrolled(r) {
			// Checked BEFORE the write so an unknown recipient costs nothing
			// durable. Note it is checked against this bus's roster only: the
			// RELAY epic is what will make a foreign "<bus>.<agent>" routable,
			// and until then a 404 is the truthful answer.
			return Result{}, fmt.Errorf("%w: %q is not enrolled on this bus", ErrUnknownRecipient, r)
		}
	}

	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	if h.poisoned != nil {
		return Result{}, h.poisoned
	}
	if h.durable == nil {
		return Result{}, ErrNotDurable
	}

	op := idem.OpSend
	if broadcast {
		op = idem.OpBroadcast
	}
	// The scope is the (agent, operation, key) tuple, never the key alone: one
	// agent can neither collide with nor probe another's keys, and cannot
	// collide with itself across two routes. The key's SHAPE was already
	// checked above (ErrInvalidIdempotencyKey, a client error), so a failure
	// here is the sender id being unusable, which is an internal fault — the
	// sender was authenticated before it reached this function.
	sc, err := idem.NewAgentScope(sender, op, key)
	if err != nil {
		return Result{}, fmt.Errorf("hub: building the idempotency scope for %s: %w", op, err)
	}
	fp := publishFingerprint(op, recipients, body)
	prev, outcome := h.idem.Lookup(sc, fp)
	switch outcome {
	case idem.OutcomeViolation:
		// Same key, DIFFERENT payload. Not a retry — a protocol violation.
		// NEITHER payload is echoed into the error: the caller already knows
		// what it sent, and the other one belongs to a message it may have no
		// business seeing quoted back.
		return Result{}, fmt.Errorf("%w: key %q was applied for message %s", ErrIdempotencyKeyReused, key, storedMessageID(prev))
	case idem.OutcomeRetry:
		// A legitimate retry: the ack was probably lost in flight. Return the
		// ORIGINAL result, re-apply nothing, error nothing, disconnect nobody.
		out, err := decodeStoredResult(prev)
		if err != nil {
			// The stored result is unreadable, so the original answer cannot be
			// returned. Failing is the only honest option: re-applying would be
			// the double-apply invariant 10 forbids.
			return Result{}, fmt.Errorf("hub: replaying the original result for idempotency key %q: %w", key, err)
		}
		out.Replayed = true
		return out, nil
	}

	// Full() expires first, so a table of keys already past the retention
	// window does not refuse a send that has room. Checked BEFORE the sequence
	// is minted: a sequence spent on a send that will be refused is a sequence
	// burned for nothing, and invariant 1 forbids reusing it.
	if h.idem.Full() {
		return Result{}, fmt.Errorf("%w: %d idempotency keys are remembered, the limit; nothing is evicted, because evicting a key turns the next retry of it into a second message", ErrCapacity, MaxIdempotencyEntries)
	}

	seq, err := h.seq.Next()
	if err != nil {
		return Result{}, fmt.Errorf("hub: allocating a message sequence: %w", err)
	}
	m, err := store.NewMessage(h.busID, sender, broadcast, recipients, seq, h.now().UTC(), body, key)
	if err != nil {
		return Result{}, err
	}
	payload, err := m.Encode()
	if err != nil {
		return Result{}, err
	}

	// THE APPLIED-KEY RECORD, built and validated BEFORE the durable write.
	//
	// CommittedAt is m.SentAt — THE SAME CLOCK READING THE MESSAGE CARRIES, not
	// a second call to h.now(). Two readings would let the message and its
	// applied key disagree about when the operation happened, and retention is
	// computed from the key's copy.
	//
	// Encoding here rather than later is what makes a record that cannot be
	// stored fail the send with NOTHING written, instead of surfacing at replay
	// when the message is already durable.
	storedResult, err := encodeStoredResult(m.ID, m.Seq, m.Recipients, m.SentAt)
	if err != nil {
		return Result{}, fmt.Errorf("hub: encoding the applied-key result for message %s: %w", m.ID, err)
	}
	idemRecord := idem.Record{
		Agent:       sender,
		Op:          op,
		Key:         key,
		Fingerprint: fp,
		Result:      storedResult,
		Seq:         m.Seq,
		CommittedAt: m.SentAt,
	}
	encodedIdem, err := idemRecord.Encode()
	if err != nil {
		return Result{}, fmt.Errorf("hub: encoding the applied-key record for message %s: %w", m.ID, err)
	}

	// THE DURABLE WRITE. Nothing below this line may run before it returns, and
	// nothing above it may be acknowledged to a client.
	//
	// Idem carries the applied-key record in the SAME two-phase transaction as
	// the message (IDEM-11's load-bearing requirement, invariant 10): a
	// wal.Entry is one transaction, so the key becomes durable when — and only
	// when — the message does, in one fsync. A separate write ordered after the
	// message would leave a window in which the message is durable and the key
	// is not; a crash there plus a client retry is a duplicate, and the window
	// is small enough to be invisible in ordinary testing.
	//
	// Audit is set non-nil to REQUEST an audit record. wal carries the field
	// today and DUR-5 writes it; store.Record is already shaped so DUR-5 lifts
	// every field invariant 6 names and drops exactly one (the body).
	committed, err := h.durable.Write(wal.Entry{
		Kind:  store.RecordKind,
		Body:  payload,
		Idem:  encodedIdem,
		Audit: &wal.AuditRecord{},
	})
	if err != nil {
		return Result{}, fmt.Errorf("hub: durably recording message %s: %w", m.ID, err)
	}

	// The id-authority assertion. See the floor derivation in Open: the whole
	// argument that a restart cannot reissue this sequence is that the sequence
	// never runs ahead of the WAL index carrying it. Checked per message rather
	// than trusted, because the counting argument is the kind of thing a future
	// edit breaks silently, and the damage — reissued message ids after a
	// restart — is undetectable downstream.
	//
	// The message is already durable at this point and cannot be unwritten, so
	// the response is to POISON the hub: this send fails, no further send is
	// accepted, and an operator gets an ERROR naming both numbers. Serving on
	// would mint ids from a floor the next start cannot reconstruct.
	if committed.PrepareIndex < seq {
		h.poisoned = fmt.Errorf("%w: message %s took sequence %d but its prepare record landed at WAL index %d; the sequence floor derived at the next start (from the log's high-water index) would then sit BELOW a sequence already written, and message ids would repeat (invariant 1)",
			ErrPoisoned, m.ID, seq, committed.PrepareIndex)
		h.log.Error("POISONED: the message sequence has overtaken the durable log index, so a restart could reissue message ids; refusing all further sends",
			"message_id", m.ID,
			"seq", seq,
			"prepare_index", committed.PrepareIndex,
		)
		return Result{}, h.poisoned
	}

	if err := h.store.Append(m); err != nil {
		// Durable but not applied: memory no longer matches disk, which is
		// exactly the divergence wal.ErrDiverged describes. Poison rather than
		// serve a store that a replay would rebuild differently.
		h.poisoned = fmt.Errorf("%w: message %s is committed on disk but was rejected by the serving copy: %s", ErrPoisoned, m.ID, err)
		h.log.Error("POISONED: a committed message could not be applied to the serving copy, so memory has diverged from disk; refusing all further sends",
			"message_id", m.ID,
			"seq", m.Seq,
			"err", err,
		)
		return Result{}, h.poisoned
	}

	result := Result{
		MessageID:  m.ID,
		Seq:        m.Seq,
		Sender:     m.Sender,
		Broadcast:  m.Broadcast,
		Recipients: m.Recipients,
		SentAt:     m.SentAt,
	}
	if err := h.idem.Remember(idemRecord); err != nil {
		// The message is COMMITTED but its applied key is not in the serving
		// table, so a retry would produce a SECOND message. That is a
		// divergence between memory and the durable record, exactly like a
		// failed store.Append above, and it gets the same answer: POISON.
		//
		// It cannot happen — Full() was checked under this same lock and Encode
		// already validated the record — but "cannot happen" is precisely the
		// class of failure that must not be allowed to corrupt the applied-key
		// table silently.
		h.poisoned = fmt.Errorf("%w: message %s is committed on disk but its applied-key record was rejected by the serving table, so a retry of key %q would produce a second message: %s", ErrPoisoned, m.ID, key, err)
		h.log.Error("POISONED: a committed message's applied-key record could not be remembered, so a retry would be applied twice; refusing all further sends",
			"message_id", m.ID,
			"seq", m.Seq,
			"err", err,
		)
		return Result{}, h.poisoned
	}

	// LAST, and only here: the message is durable and it is in the serving
	// copy, so a waiter woken now cannot observe something a crash would take
	// back (POLL-2).
	h.notify(m)
	return result, nil
}

// IdempotencyStats reports the observable state of the applied-key table: how
// many keys are retained, how old the oldest is, how many have been evicted,
// and the bounds in force.
//
// It is IDEM-11 point (g)'s hook — the thing CORE-5's inspect/metrics endpoint
// surfaces so the bound is VERIFIED in production rather than assumed. Watching
// Stats.OldestAge approach Stats.Window is how an operator sees the derived
// retention margin actually being consumed, and Stats.Expired counts the
// evictions past which a retry is no longer suppressed.
func (h *Hub) IdempotencyStats() idem.Stats {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return h.idem.Stats()
}

// publishFingerprint digests the semantic content of a send: what operation it
// is, who it is addressed to, and what it says. Two requests with the same
// fingerprint are the same request for the purposes of invariant 10.
//
// THE FIELD LIST AND ORDER, documented here as idem's doc.go point 8 requires
// every call site to do:
//
//	[ op ("send" | "broadcast"),
//	  8-byte big-endian recipient count,
//	  each recipient, in order,
//	  body ]
//
// The op string subsumes the old "broadcast"/"direct" discriminator: it is
// already the scope's operation component, so hashing it keeps the digest
// unambiguous without carrying a second, parallel spelling of the same fact.
//
// EVERY FIELD IS LENGTH-PREFIXED by idem.ComputeFingerprint, so ("ab","c") and
// ("a","bc") cannot digest alike. The recipient COUNT is hashed in addition,
// because the count is what distinguishes a directed send to N agents from one
// to N-1 with a differently-split list.
//
// NOTE FOR ANY FUTURE CHANGE TO THIS LIST: since IDEM-11 this digest is STORED
// ON DISK inside the applied-key record rather than recomputed at replay, so
// changing the field list changes the MEANING of records already written — an
// old record's fingerprint would no longer match the digest a retry of the same
// request now produces, and the retry would be reported as a key-reuse
// VIOLATION rather than replayed. Anything that changes this must therefore
// carry a migration, not just a code change.
func publishFingerprint(op idem.Operation, recipients []string, body []byte) idem.Fingerprint {
	fields := make([][]byte, 0, len(recipients)+3)
	fields = append(fields, []byte(op))
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(recipients)))
	fields = append(fields, count[:])
	for _, r := range recipients {
		fields = append(fields, []byte(r))
	}
	fields = append(fields, body)
	return idem.ComputeFingerprint(fields...)
}

// storedResult is what an applied-key record retains of a send's result.
//
// It stores ONLY what the scope does not already say. The scope tuple carries
// the agent and the operation, so Result.Sender is rebuilt from Record.Agent
// and Result.Broadcast from Record.Op == idem.OpBroadcast; storing either again
// would spend part of a 512-byte budget (idem.MaxResultBytes) restating a fact
// the record already carries, and would create a second copy that could
// disagree with the first.
type storedResult struct {
	MessageID  string   `json:"message_id"`
	Seq        uint64   `json:"seq"`
	Recipients []string `json:"recipients,omitempty"`
	SentAt     string   `json:"sent_at"`
}

// encodeStoredResult renders the retained half of a Result.
func encodeStoredResult(messageID string, seq uint64, recipients []string, sentAt time.Time) (json.RawMessage, error) {
	b, err := json.Marshal(storedResult{
		MessageID:  messageID,
		Seq:        seq,
		Recipients: recipients,
		SentAt:     sentAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// decodeStoredResult rebuilds the full Result a retry is answered with, filling
// the scope-derived fields back in from the record itself.
func decodeStoredResult(rec idem.Record) (Result, error) {
	var sr storedResult
	if err := json.Unmarshal(rec.Result, &sr); err != nil {
		return Result{}, err
	}
	sentAt, err := time.Parse(time.RFC3339Nano, sr.SentAt)
	if err != nil {
		return Result{}, err
	}
	return Result{
		MessageID:  sr.MessageID,
		Seq:        sr.Seq,
		Sender:     rec.Agent,
		Broadcast:  rec.Op == idem.OpBroadcast,
		Recipients: sr.Recipients,
		SentAt:     sentAt.UTC(),
	}, nil
}

// storedMessageID digs just the message id out of a stored result, for the
// key-reuse error. It never fails: a record whose result will not decode still
// has to produce a usable violation message, and "unknown" is more honest than
// swallowing the violation.
func storedMessageID(rec idem.Record) string {
	var sr storedResult
	if err := json.Unmarshal(rec.Result, &sr); err != nil || sr.MessageID == "" {
		return "unknown"
	}
	return sr.MessageID
}

// validateIdempotencyKey enforces the shape of a client-supplied key:
// non-empty, at most MaxIdempotencyKeyLen bytes, [A-Za-z0-9._-] only. It is the
// same rule internal/auth applies, and for the same reason — the key is
// reflected into the server log.
func validateIdempotencyKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: an idempotency key is required on every mutating call so a retry after a lost acknowledgement cannot be applied twice (invariant 10)", ErrInvalidIdempotencyKey)
	}
	if len(key) > MaxIdempotencyKeyLen {
		// Not echoed: it is oversized, and an attacker choosing the input must
		// not choose a multiple of it back out of a log line.
		return fmt.Errorf("%w: %d bytes, but an idempotency key is at most %d; the key is not echoed here because it is oversized", ErrInvalidIdempotencyKey, len(key), MaxIdempotencyKeyLen)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '-':
		default:
			return fmt.Errorf("%w: byte %d is %q, but an idempotency key must contain only [A-Za-z0-9._-]", ErrInvalidIdempotencyKey, i, key[i:i+1])
		}
	}
	return nil
}

// Poisoned reports the error that stopped this hub accepting writes, or nil.
func (h *Hub) Poisoned() error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return h.poisoned
}
