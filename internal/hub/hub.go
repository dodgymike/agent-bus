package hub

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

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
// It is a MEMORY bound, not the retention policy. The real retention is the
// message window (store.DefaultMaxAge / store.DefaultMaxBytes): a key is
// forgotten when the message it produced ages out of the store, which is
// exactly the right window — a key is worth remembering for as long as a client
// could plausibly still be retrying the send it belongs to, and no longer.
//
// The table FAILS CLOSED at this bound rather than evicting. Evicting a
// remembered key silently turns the next retry of it into a SECOND message,
// which is the double-apply invariant 10 forbids; a refused send is
// recoverable, a duplicated one is not. It is the same posture as
// auth.recordIdempotent, and it is reached only if more than this many messages
// are inside the retention window at once.
const MaxIdempotencyEntries = 1 << 16

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

	// idem is the applied-key table, guarded by writeMu. It is keyed on the
	// SENDER AND the key together: an idempotency key is scoped to the agent
	// that chose it, so one agent can neither collide with nor probe for
	// another's keys.
	idem map[idemKey]idemEntry

	// idemOrder is the same keys in sequence order, so expiry is a pop from the
	// front rather than a scan. Guarded by writeMu.
	idemOrder []idemAge

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

// idemKey scopes an idempotency key to the agent that chose it.
type idemKey struct {
	sender string
	key    string
}

// idemEntry is one remembered send: enough of the request to tell a legitimate
// RETRY from a key REUSED for different content, plus the original result to
// replay.
type idemEntry struct {
	// fingerprint is a SHA-256 over the request's semantic content. The whole
	// payload is not retained: a body may be 64 KiB and the table may hold tens
	// of thousands of entries, and a 32-byte digest answers the only question
	// asked of it — "is this the same request?" — for a fraction of the memory.
	//
	// This is a COLLISION-RESISTANCE use of a hash, not a secret-keyed one: two
	// DIFFERENT payloads colliding here would make a protocol violation look
	// like a retry. SHA-256 is the right tool and there is no bespoke
	// construction anywhere near it (invariant 9).
	fingerprint [sha256.Size]byte
	result      Result
}

// idemAge pairs a key with the sequence of the message it produced, so the
// table can be expired against the store's retention window.
type idemAge struct {
	key idemKey
	seq uint64
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
		idem:           make(map[idemKey]idemEntry),
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
	// EXPIRED AND BOUNDED on this path exactly as on the live one. Without
	// this, replay builds one entry per message ever written — the WAL does not
	// compact — so a long-lived bus turns a bounded steady state into an
	// unbounded STARTUP allocation. The store has already pruned by the time we
	// get here, so the retention cutoff is the same one publish would compute.
	h.expireIdemLocked()
	if m.IdempotencyKey != "" && len(h.idem) < MaxIdempotencyEntries {
		h.rememberLocked(idemKey{sender: m.Sender, key: m.IdempotencyKey}, idemEntry{
			fingerprint: fingerprint(m.Broadcast, m.Recipients, m.Body),
			result: Result{
				MessageID:  m.ID,
				Seq:        m.Seq,
				Sender:     m.Sender,
				Broadcast:  m.Broadcast,
				Recipients: m.Recipients,
				SentAt:     m.SentAt,
			},
		})
	}
	return nil
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

	ik := idemKey{sender: sender, key: key}
	fp := fingerprint(broadcast, recipients, body)
	if prev, ok := h.idem[ik]; ok {
		if prev.fingerprint != fp {
			// Same key, DIFFERENT payload. Not a retry — a protocol violation.
			// Neither payload is echoed into the error: the caller already
			// knows what it sent, and the other one belongs to a message it may
			// have no business seeing quoted back.
			return Result{}, fmt.Errorf("%w: key %q was applied for message %s", ErrIdempotencyKeyReused, key, prev.result.MessageID)
		}
		// A legitimate retry: the ack was probably lost in flight. Return the
		// ORIGINAL result, re-apply nothing, error nothing, disconnect nobody.
		out := prev.result
		out.Replayed = true
		return out, nil
	}

	// Expiry first, so a table full of keys whose messages have already aged
	// out does not refuse a send that has room.
	h.expireIdemLocked()
	if len(h.idem) >= MaxIdempotencyEntries {
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

	// THE DURABLE WRITE. Nothing below this line may run before it returns, and
	// nothing above it may be acknowledged to a client.
	//
	// Audit is set non-nil to REQUEST an audit record. wal carries the field
	// today and DUR-5 writes it; store.Record is already shaped so DUR-5 lifts
	// every field invariant 6 names and drops exactly one (the body).
	committed, err := h.durable.Write(wal.Entry{
		Kind:  store.RecordKind,
		Body:  payload,
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
	h.rememberLocked(ik, idemEntry{fingerprint: fp, result: result})

	// LAST, and only here: the message is durable and it is in the serving
	// copy, so a waiter woken now cannot observe something a crash would take
	// back (POLL-2).
	h.notify(m)
	return result, nil
}

// rememberLocked records an applied key. The caller must hold writeMu, or be
// inside Open before the hub is reachable.
func (h *Hub) rememberLocked(k idemKey, e idemEntry) {
	if _, ok := h.idem[k]; ok {
		return
	}
	h.idem[k] = e
	h.idemOrder = append(h.idemOrder, idemAge{key: k, seq: e.result.Seq})
}

// expireIdemLocked forgets keys whose messages have aged out of the store.
// The caller must hold writeMu.
//
// Tying key retention to MESSAGE retention is the point: a key is worth
// remembering exactly as long as the message it produced is still part of this
// bus's history, and no longer. It also means the table cannot outgrow the
// store, so the hard cap above it is a memory backstop rather than the policy.
func (h *Hub) expireIdemLocked() {
	count, _, oldest, head, _ := h.store.Stats()
	// An EMPTY store is not "nothing to expire". When count is 0 but head is
	// not, retention has taken EVERY message: the honest cutoff is then one
	// past the head, so every remembered key goes with the messages it belongs
	// to. Reading `oldest == 0` as "skip" — the obvious spelling — would leak
	// the whole table on exactly the bus this bound exists for: one that went
	// quiet long enough for its history to age out.
	cutoff := oldest
	if count == 0 {
		if head == 0 {
			return // a fresh store; there is nothing to expire against
		}
		cutoff = head + 1
	}
	drop := 0
	for drop < len(h.idemOrder) && h.idemOrder[drop].seq < cutoff {
		delete(h.idem, h.idemOrder[drop].key)
		drop++
	}
	if drop == 0 {
		return
	}
	kept := make([]idemAge, len(h.idemOrder)-drop)
	copy(kept, h.idemOrder[drop:])
	h.idemOrder = kept
}

// fingerprint digests the semantic content of a send: what it says and who it
// is addressed to. Two requests with the same fingerprint are the same request
// for the purposes of invariant 10.
//
// The parts are LENGTH-PREFIXED rather than concatenated, so no two different
// requests can produce the same input bytes — ("ab","c") and ("a","bc") must
// not fingerprint alike, and plain concatenation is precisely how that kind of
// ambiguity gets built.
func fingerprint(broadcast bool, recipients []string, body []byte) [sha256.Size]byte {
	h := sha256.New()
	var b [8]byte
	writeField := func(s []byte) {
		binary.BigEndian.PutUint64(b[:], uint64(len(s)))
		_, _ = h.Write(b[:])
		_, _ = h.Write(s)
	}
	if broadcast {
		writeField([]byte("broadcast"))
	} else {
		writeField([]byte("direct"))
	}
	binary.BigEndian.PutUint64(b[:], uint64(len(recipients)))
	_, _ = h.Write(b[:])
	for _, r := range recipients {
		writeField([]byte(r))
	}
	writeField(body)

	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
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
