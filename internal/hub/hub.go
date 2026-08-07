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

// maxDecodeFailuresLogged bounds how many undecodable message records recovery
// names INDIVIDUALLY. It mirrors internal/wal's maxDanglingLogged, which caps
// the same class of flood one layer down and for the same reason: a bulk
// discard — a record-schema bump discards every message record in the log — must
// not bury the rest of the startup log under one line per record. The exact
// total is never lost; noteRecoveredIdentities reports it.
const maxDecodeFailuresLogged = 8

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

	// DataDir is the directory holding this bus's durable state. It is where
	// the MESSAGE-SEQUENCE FLOOR file (SeqFloorFileName) is read and written —
	// the only record of a minted sequence that survives a WAL quarantine.
	//
	// It is REQUIRED whenever Durable is set, and Open FAILS without it. That is
	// not fussiness about a field: a hub that can mint but cannot write the floor
	// file would burn its numbers only inside the log, and a quarantine would
	// then hand the SAME sequence to a second client — the exact P0 seqfloorfile.go
	// exists to close. There is no "best effort" setting for it, for the same
	// reason invariant 4 has none.
	//
	// A hub built with Durable nil (a read-only or test hub, which refuses every
	// send and every mint with ErrNotDurable) may leave it empty: it can never
	// issue a number, so there is nothing to burn.
	DataDir string

	// Roster is who is enrolled on this bus. It is REQUIRED and there is NO
	// DEFAULT — Open FAILS on a nil one.
	//
	// An empty default would be catastrophically quiet. Every roster check in
	// this package fails CLOSED, so a hub built with nothing to read would
	// answer 403 to every send, 404 to every recipient and an empty agent list
	// to everyone, while starting, logging nothing and passing a health check.
	// "Serves nobody" must not be reachable by forgetting a field, which is why
	// the omission is a startup error and not a warning.
	//
	// It is READ THROUGH on every check, never copied: see RosterSource.
	Roster RosterSource

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

	// MaxIdempotencyEntries bounds the applied-key table for THIS hub. 0 means
	// idem's derived default, which is the MaxIdempotencyEntries CONSTANT in
	// this package (65536). The field and the constant share a name because
	// they are the same bound at different scopes — a Go field and a package
	// constant do not collide — and 0 here is exactly that constant.
	//
	// WHY IT IS CONFIGURABLE AT ALL, stated honestly: it is not a tuning knob
	// anybody is expected to turn in production. It exists because the
	// per-agent FAIR SHARE (IDEM-11-FU-FAIRSHARE) is only exercisable by
	// actually FILLING a table, and filling the real 65536-entry table means
	// 65536 durable, fsynced writes — a test nobody will run, and therefore a
	// security property nobody would ever check. A configurable bound is what
	// makes the property PROVABLE rather than asserted.
	MaxIdempotencyEntries int
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
	// The ROSTER's lock is deliberately NOT in that chain: publish validates the
	// sender and the recipients BEFORE it takes writeMu, so the two are never
	// held together at all. That is the strongest position available and it is
	// worth keeping — but it is also why Enrolled's TOCTOU note matters, so read
	// that before moving the roster check inside the lock to "tidy this up".
	// Since AUTH-7 that lock belongs to an INJECTED RosterSource, so this
	// package cannot even see it; nothing here may hold writeMu across a call
	// into the roster.
	//
	// Nothing takes writeMu while holding waitMu, the roster's lock or the store
	// lock, so there is no cycle. The read paths (History, Wait, Agents) never
	// take writeMu at all, which is what keeps a long poll off the fsync path.
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

	// idemMaxEntries is the applied-key bound actually in force — the constant
	// MaxIdempotencyEntries unless Options.MaxIdempotencyEntries overrode it.
	// Set once in Open, read-only afterwards; it exists so a refusal quotes the
	// bound this hub is running with rather than the compiled-in default.
	idemMaxEntries int

	// idemCapWarned records that the replay-time capacity warning has already
	// been emitted, so a large log produces ONE line rather than one per message.
	// Written only during Open, before the hub is reachable.
	//
	// There is deliberately no companion flag for the per-agent fair share:
	// replay calls idem.Store.Recover, which does not adjudicate the share at
	// all, so no replayed record can be refused for it BY CONSTRUCTION.
	idemCapWarned bool

	// poisoned, once set, is never cleared. See ErrPoisoned. Guarded by
	// writeMu.
	poisoned error

	// appliedSeq is the highest sequence seen by Apply during recovery. It is
	// written only during Open, before anything can reach the hub, and read
	// only there.
	appliedSeq uint64

	// undecodableMessages counts the message records recovery DISCARDED because
	// they would not decode. Written only during Open, before anything can reach
	// the hub.
	//
	// It exists so noteRecoveredIdentities can distinguish "no ids were
	// recovered because this directory is fresh" from "no ids were recovered
	// because every record was discarded" — two states that are identical in the
	// recovered map and opposite in what they mean.
	undecodableMessages int

	// replayedSeqFloor is the highest floor claimed by a SeqFloorRecordKind
	// record replayed from disk (see mint.go). Written only during Open, by
	// applySeqFloor, before anything can reach the hub.
	replayedSeqFloor uint64

	// seqFloorFile is the DURABLE, quarantine-proof message-sequence floor:
	// <data-dir>/message-seq-floor, outside the log. It is the AUTHORITATIVE
	// source of the floor — see seqfloorfile.go for why the in-log "seqfloor"
	// record it supplements cannot be. Nil only on a hub with no durable write
	// path, which can never issue a sequence at all.
	//
	// It has its own lock, so it is NOT guarded by writeMu; but every hub caller
	// reaches it under writeMu anyway, because the number it authorises is
	// allocated under writeMu.
	seqFloorFile *seqFloorFile

	// durableSeqFloor is the highest sequence this bus has DURABLY RECORDED as
	// burned. Every number handed out must be at or below it — that is the
	// assertion which replaced Open's counting argument, and assertSeqFloorLocked
	// is where it is enforced.
	//
	// It is set once in Open, from the same maximum the sequence allocator is
	// sealed at, and raised thereafter ONLY by ensureSeqFloorLocked, which raises
	// it only AFTER the record proving it is fsynced. Guarded by writeMu. It
	// never decreases; a lower value would claim numbers are available that have
	// already been handed out.
	durableSeqFloor uint64

	// mints is every sequence reservation handed out and not yet spent, keyed by
	// the (agent, operation, idempotency key) tuple; mintsByAgent is the same set
	// counted per agent, for the per-agent bound. They are updated in the same
	// critical section, under writeMu, and must never drift.
	//
	// IN MEMORY ONLY, DELIBERATELY. What must survive a restart is that the
	// NUMBER is burned, and the durable floor record does that; which client
	// happened to be holding it does not need to survive, because a client that
	// loses its reservation re-mints under the same idempotency key and cannot
	// double-apply (see ErrUnknownMint). Making this table durable would put a
	// second fsync on the mint path to protect a fact nothing depends on.
	mints        map[mintKey]outstandingMint
	mintsByAgent map[string]int

	// roster is the AUTHORITATIVE roster, injected and read through. The hub
	// keeps no copy of its own — see RosterSource for why a copy is a defect and
	// not an optimisation. Set once in Open, never replaced.
	roster RosterSource

	// recovered holds every agent id named as a sender or a recipient by a
	// message replayed from disk at startup. It is written only during Open and
	// read-only afterwards, so it needs no lock.
	//
	// It exists to make ONE thing observable: an id that appears here and is
	// NOT in the roster is an id whose holder is gone and whose name could be
	// minted again, which invariant 1 forbids. See noteRecoveredIdentities.
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
	if o.Roster == nil {
		// HARD, not a warning, and not a silent empty roster. See Options.Roster:
		// every check in this package fails closed, so the omission would produce
		// a bus that authenticates everyone and serves nobody, quietly.
		return nil, errors.New("hub: open: a roster source is REQUIRED and has no default; a hub with nothing to read would refuse every send, reject every recipient and serve an empty agent list while looking healthy (see hub.RosterSource)")
	}
	if o.Durable != nil && o.DataDir == "" {
		// HARD, and for the same reason the roster check above is hard: the
		// failure it prevents is SILENT. A durable hub without a data directory
		// would mint happily, burn its numbers only inside the log, and reissue
		// every one of them after a quarantine — with both copies validly signed
		// and nothing downstream able to tell. See Options.DataDir.
		return nil, errors.New("hub: open: a data directory is REQUIRED for a hub with a durable write path; it is where the message sequence floor (" + SeqFloorFileName + ") lives, and without it a WAL quarantine would reissue sequence numbers already handed to — and already signed by — a client (invariant 1)")
	}
	h := &Hub{
		busID:          o.BusID,
		durable:        o.Durable,
		log:            o.Logger,
		now:            o.Now,
		pollTimeout:    o.PollTimeout,
		roster:         o.Roster,
		recovered:      make(map[string]struct{}),
		waiters:        make(map[*waiter]struct{}),
		waitersByAgent: make(map[string]int),
		mints:          make(map[mintKey]outstandingMint),
		mintsByAgent:   make(map[string]int),
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
	h.idem = idem.NewStore(idem.StoreOptions{Now: h.now, MaxEntries: o.MaxIdempotencyEntries})
	// Read the bound BACK from the store rather than re-deriving the 0-means-
	// default rule here: one place decides what the bound is, and the refusals
	// below then quote the bound actually in force instead of the constant.
	h.idemMaxEntries = h.idem.Stats().MaxEntries

	// # Deriving the sequence floor — read this before changing it
	//
	// invariant 1: a sequence number is NEVER reused, including across
	// restarts. ids.Resume wants "the highest sequence EVER WRITTEN TO DISK —
	// prepared or committed, delivered or discarded", and it cannot check the
	// claim, so the claim has to be argued here.
	//
	// The floor is the MAXIMUM of four durable facts, each of which is on disk
	// and each of which alone would be sound in the situations it covers:
	//
	//	(0) the durable message-sequence floor FILE       (seqfloorfile.go)
	//	(1) the durable log's high-water index, NextIndex-1
	//	(2) the highest floor claimed by a replayed "seqfloor" record (mint.go)
	//	(3) the highest sequence carried by a replayed message record
	//
	// # (0) IS THE AUTHORITATIVE ONE. (1), (2) AND (3) ARE DEFENCE IN DEPTH
	//
	// Sources (1), (2) and (3) are all read OUT OF THE LOG, and the log is the
	// one artifact recovery is allowed to truncate, rewrite or move aside
	// wholesale. So all three drop together, to nearly zero, on exactly the start
	// where the floor matters most — a quarantine. Since the mint burns numbers
	// in BATCHES of MintBatchSize, a sequence is no longer bounded by the WAL
	// index of any record (five mints consume five sequences and two indices), so
	// wal's own durable index floor no longer covers this one transitively
	// either. Source (0) lives in its OWN atomically-replaced file outside the
	// log, which is what makes it survive; read seqfloorfile.go before touching
	// any of this.
	//
	// # (2) IS THE LOAD-BEARING ONE SINCE SIGN-6, AND (1) IS NO LONGER SUFFICIENT
	//
	// (1) used to stand alone, on a COUNTING argument: every sequence issued was
	// <= the WAL index of the prepare carrying it, because each message consumed
	// one sequence and at least two indices, so the indices outran the sequences
	// for ever. That argument is RETIRED. SIGN-1 settled that the sender signs
	// the origin bus's minted id and sequence, so /v1/mint now hands a sequence
	// to a client BEFORE any record carries it — a sequence consumed against
	// ZERO indices — and the counting stops holding on the very first mint.
	//
	// What replaced it is not another derivation but a WRITTEN CLAIM: the mint
	// fsyncs a "seqfloor" record burning a batch of numbers BEFORE it hands any
	// of them out, and (2) reads those records back. See mint.go for the whole
	// argument and for the assertion (assertSeqFloorLocked) that now enforces it
	// at both ends.
	//
	// (1) is KEPT anyway, as defence in depth: it can only ever RAISE the floor,
	// never lower it, and it is the fallback if a floor record is ever
	// undecodable at replay (applySeqFloor discards such a record LOUDLY and
	// carries on, exactly as invariant 6 requires).
	//
	// (3) is likewise kept and is likewise only ever a raise. Note it must NOT
	// be used ALONE: it misses the sequence of a DANGLING prepare (a message
	// prepared but never committed, the ordinary signature of a crash mid-write),
	// which never reaches Apply, and reissuing it is exactly what invariant 1's
	// 2026-08-02 restatement forbids — "recovery may not reissue an index it has
	// already handed out, even for a record it discards".
	//
	// floor is tracked in this local as well as inside the allocator because
	// h.durableSeqFloor must end up at the SAME value the allocator is sealed
	// at, and ids.Sequence exposes no floor accessor by design. Every RaiseFloor
	// below therefore updates both, in one place each.
	// Source (0), read FIRST so every line below — including the quarantine
	// report — can see it. A corrupt file is FATAL and is never regenerated (see
	// ErrSeqFloorFileCorrupt); a MISSING one is not, and is reported below.
	if o.DataDir != "" {
		sf, err := openSeqFloorFile(o.DataDir)
		if err != nil {
			return nil, err
		}
		h.seqFloorFile = sf
	}

	floor := o.NextIndex
	if floor > 0 {
		floor--
	}
	if h.seqFloorFile != nil && h.seqFloorFile.burned() > floor {
		floor = h.seqFloorFile.burned()
	}
	h.seq = ids.Resume(floor)

	if o.Quarantined != "" {
		// LOUD, and not a nicety. A quarantine starts a FRESH log, so NextIndex
		// restarts near 1 while the quarantined file holds sequences far above
		// it. Whether that is survivable depends ENTIRELY on source (0), so the
		// two cases are reported differently — a single unconditional sentence
		// here would be false on one of them, and an operator reading a false
		// startup ERROR learns to ignore the true one.
		switch {
		case h.seqFloorFile != nil && h.seqFloorFile.existedAtOpen():
			h.log.Error("the durable log was QUARANTINED. Message ids are NOT at risk: the durable message sequence floor lives outside the log and survived, so this bus resumes strictly above every sequence it has ever handed out (invariant 1). The MESSAGES in the quarantined file are another matter — that file is the only record of them",
				"quarantined_to", o.Quarantined,
				"seq_floor_file", h.seqFloorFile.Path(),
				"resumed_floor", floor,
			)
		default:
			h.log.Error("the durable log was QUARANTINED and there is NO durable message sequence floor file, so the sequence high-water mark from before the quarantine is UNKNOWN: message ids may repeat values the quarantined log already used, and a client may hold a signature over one of them (invariant 1). This is the one-start migration window for a data directory written before "+SeqFloorFileName+" existed; the file is written on this start, so the next one is covered",
				"quarantined_to", o.Quarantined,
				"resumed_floor", floor,
			)
		}
	}

	if o.Replay != nil {
		rec, err := o.Replay(h.Apply)
		if err != nil {
			// Recovery could not complete. Memory is not a prefix of anything
			// and must not be served (invariant 5).
			return nil, fmt.Errorf("hub: rebuilding the message store from the durable log: %w", err)
		}
		if rec.NextIndex > 0 && rec.NextIndex-1 > floor {
			floor = rec.NextIndex - 1
			if err := h.seq.RaiseFloor(floor); err != nil {
				return nil, fmt.Errorf("hub: raising the sequence floor to the replayed high-water mark: %w", err)
			}
		}
		h.log.Info("message store rebuilt from the durable log",
			"messages", h.store.Head(),
			"applied_high_water", h.appliedSeq,
			"seq_floor_records_high_water", h.replayedSeqFloor,
			"records_replayed", rec.Records,
			"next_index", rec.NextIndex,
		)
	}

	// Compare what the messages remember against who is actually enrolled. It
	// runs HERE, after replay has filled h.recovered and while nothing can yet
	// reach this hub, and it assumes the injected roster has ALREADY recovered —
	// which is a real ordering requirement on the caller, not an implementation
	// detail: cmd/agent-bus opens the durable log (whose replay rebuilds the
	// roster) strictly before it opens the hub.
	h.noteRecoveredIdentities()

	// Source (2): every sequence a replayed "seqfloor" record says was burned
	// STAYS burned, whether or not a message ever carried it. This is the one
	// that covers the numbers handed out by /v1/mint and never spent — the case
	// no other source here can see, because nothing was written about them
	// anywhere else.
	if h.replayedSeqFloor > floor {
		floor = h.replayedSeqFloor
		if err := h.seq.RaiseFloor(floor); err != nil {
			return nil, fmt.Errorf("hub: raising the sequence floor to the highest durably-burned sequence: %w", err)
		}
	}

	// Source (3), belt and braces: never resume at or below a sequence a
	// replayed message record proves was written.
	if h.appliedSeq > floor {
		floor = h.appliedSeq
		if err := h.seq.RaiseFloor(floor); err != nil {
			return nil, fmt.Errorf("hub: raising the sequence floor to the highest replayed sequence: %w", err)
		}
	}
	// PERSIST THE DERIVED FLOOR BEFORE ANYTHING MAY ISSUE. This is the exact
	// counterpart of wal indexFloor.begin's "written = start-1", and it is the
	// step most easily mistaken for redundant bookkeeping.
	//
	// It is not. Sources (1), (2) and (3) are read out of the log; source (0) is
	// the only one that survives a quarantine. A run that derived a high floor
	// from the LOG and did not write it down leaves no trace of what it resumed
	// at, so the NEXT start — the one where the log is quarantined — would fall
	// back to a file that still says whatever it said before, and reissue the
	// whole range in between. Writing the maximum here is what makes each start's
	// knowledge survive the loss of the log it came from.
	//
	// A failure is FATAL: a floor that is not on disk is a floor that does not
	// exist, and serving on would mean handing out numbers this bus cannot prove
	// it has never handed out before. Nothing has been issued at this point, so
	// refusing costs nothing but a restart.
	//
	// raise(0) on a genuinely fresh data directory is a no-op and writes NOTHING,
	// so a first start leaves no file behind until the first mint burns a batch.
	if h.seqFloorFile != nil {
		migrating := !h.seqFloorFile.existedAtOpen() && floor > 0
		if err := h.seqFloorFile.raise(floor); err != nil {
			return nil, fmt.Errorf("hub: recording the durable message sequence floor before serving: %w", err)
		}
		if migrating {
			// The MIGRATION window, named. A data directory with history but no
			// floor file was written by a binary that predates the file, and until
			// this write landed a quarantine could have reissued a minted
			// sequence. It is a WARN and not an ERROR because the window is now
			// CLOSED — one start, and the derivation it was seeded from is the
			// best the log can prove.
			h.log.Warn("this data directory had no durable message sequence floor file: it was written by an agent-bus that predates it. Until now a WAL quarantine could have reissued sequence numbers already handed out by /v1/mint. The file has been created from the floor the log proves, so this start closes the window",
				"seq_floor_file", h.seqFloorFile.Path(),
				"floor", floor,
			)
		}
	}

	// Floor assembly ends here. Next refuses to issue until it does.
	if err := h.seq.Seal(); err != nil {
		return nil, fmt.Errorf("hub: sealing the sequence floor: %w", err)
	}

	// The sealed floor is, by construction, the highest number this bus can
	// PROVE from disk is burned — so it is also the starting value of the
	// durable floor the mint asserts against. Setting it to anything lower would
	// make the very first mint's assertion fire on a hub that is perfectly
	// healthy; setting it HIGHER would claim numbers are burned that nothing on
	// disk says are, which is the silent id-reuse this whole derivation exists
	// to prevent. The first mint then writes the first record ABOVE it (see
	// ensureSeqFloorLocked), which is what makes the claim durable going forward.
	h.durableSeqFloor = floor
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
// # THAT IS TRUE ONLY BECAUSE main PASSES A NIL Applier — read this before the
// # migration described in the ReplayFunc comment
//
// wal.Applier's own contract says the opposite: once a Hub is registered as
// wal.LogOptions.Applier, Apply is called for LIVE commits too, and it "cannot
// tell them apart" (internal/wal/log.go). Today cmd/agent-bus/main.go passes
// Applier: nil and reaches this only through the Options.Replay closure, so
// "runs during recovery" holds.
//
// It must not be allowed to stop holding by accident. Apply inserts through
// idem.Store.Recover, which DELIBERATELY SKIPS the per-agent fair share because
// a record on disk is proof that admission already succeeded (see Recover's
// doc). On a live commit that exemption is wrong: it would admit past the share
// and make publish's own Admit/Remember pair — and the poison guard that backs
// it — dead code. Whoever performs that migration must split this function by
// call site FIRST: Recover for replay, Remember for live.
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
//
// TWO kinds are understood here, and they are not interchangeable:
// store.RecordKind is a MESSAGE and rebuilds the serving copy;
// SeqFloorRecordKind is a claim that a range of sequence numbers is burned and
// contributes only to the sequence floor (see mint.go). A floor record carries
// no message and must never be allowed to reach store.Decode, which would
// report it as damage.
func (h *Hub) Apply(c wal.Committed) error {
	if c.Entry.Kind == SeqFloorRecordKind {
		return h.applySeqFloor(c)
	}
	if c.Entry.Kind != store.RecordKind {
		return nil
	}
	m, err := store.Decode(c.Entry.Body)
	if err != nil {
		// COUNTED, not merely logged. The count is what noteRecoveredIdentities
		// needs to tell "this data directory is fresh" from "everything was
		// discarded", which look identical from the recovered-id map alone — see
		// the comment there. Without it, the first start after a record-schema
		// change silently disables the id-reuse detector.
		h.undecodableMessages++
		// Logged per record only up to a cap. A schema bump discards EVERY
		// message record in the log, so an uncapped line here is one ERROR per
		// message on exactly the start an operator most needs to be able to read
		// — the same flood internal/wal already caps one layer down, and the same
		// one-shot shape used for the applied-key capacity warning below. The
		// exact total is not lost: noteRecoveredIdentities reports it.
		if h.undecodableMessages <= maxDecodeFailuresLogged {
			h.log.Error("DISCARDING a message record that could not be decoded during recovery; it is not in this bus's history and will not be delivered",
				"prepare_index", c.PrepareIndex,
				"commit_index", c.CommitIndex,
				"discarded_so_far", h.undecodableMessages,
				"err", err,
			)
			if h.undecodableMessages == maxDecodeFailuresLogged {
				h.log.Error("further undecodable message records will NOT be logged individually; the total is reported once recovery finishes",
					"logged_up_to", maxDecodeFailuresLogged,
				)
			}
		}
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
	// It is Recover, NOT Remember, and that is load-bearing: Recover omits the
	// per-agent fair share (idem.Store.Recover). Replay is not ADMITTING
	// anything — every record here was already admitted, acknowledged and
	// fsynced by the run that accepted it — and re-adjudicating an accepted
	// record can only make two runs of the same log disagree, silently dropping
	// a key whose next retry would then become a SECOND message.
	if err := h.idem.Recover(rec); err != nil {
		if errors.Is(err, idem.ErrCapacity) {
			// Not fatal, but the operator must know: the rebuilt table holds the
			// records replay reached FIRST and none after the cap — a prefix of
			// the durable log's applied keys in commit order, minus whatever the
			// retention window had already expired. Keys beyond the cap will not
			// suppress a retry that the pre-restart bus would have suppressed.
			// Logged ONCE — one line per message would bury it.
			if !h.idemCapWarned {
				h.idemCapWarned = true
				h.log.Warn("the applied-key table reached its cap during recovery, so the rebuilt table holds only the applied keys replay reached BEFORE the cap (a prefix in commit order, less whatever the retention window had already expired): keys beyond the cap will not suppress a retry",
					"max_entries", h.idemMaxEntries,
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

// SignedMint is the half of a send request that the client obtained from
// /v1/mint and then SIGNED. It is embedded in both BroadcastRequest and
// SendRequest so the two cannot drift: a shape that is mandatory on one route
// and optional on the other is the unsigned-traffic hole SIGN-6 exists to close.
//
// EVERY FIELD HERE IS CLIENT INPUT TO BE VALIDATED, NEVER AN IDENTITY OR AN
// ASSIGNMENT TO BE TRUSTED (invariant 1). MessageID and Seq are checked against
// the reservation this bus minted and the RESERVATION wins; Signature is checked
// for SHAPE only and is then carried as opaque bytes.
type SignedMint struct {
	// MessageID and Seq are the assignment the client is presenting back. They
	// must be exactly what Mint returned for this (sender, operation, key), or
	// the send is ErrMintMismatch.
	MessageID string
	Seq       uint64

	// TimestampUnixMilli is the SENDER's clock, and is covered by the signature.
	// It is NOT this bus's clock and does not order anything — see
	// store.Message.TimestampUnixMilli.
	TimestampUnixMilli int64

	// Signature is the detached Ed25519 signature over
	// signing.Canonicalize(store.Message.SigningMessage()). The bus checks its
	// LENGTH and never verifies it: it does not hold the sender's messaging key
	// and must not be trusted to police messages for senders it does not
	// control.
	Signature []byte
}

// BroadcastRequest is one broadcast attempt. Sender is the AUTHENTICATED
// principal and is supplied by the caller from the request context, never from
// the request body (invariant 1).
//
// NOTE: /v1/broadcast currently answers 501 and never reaches here — a broadcast
// has no canonical audience under signing format v1, because
// signing.Canonicalize rejects an empty recipient set and store.Message stores a
// broadcast as a FLAG rather than an expanded roster snapshot. The hub-level
// plumbing is deliberately kept whole and signed-by-construction so SIGN-3 can
// re-open the route by settling that one question, not by re-plumbing this path.
type BroadcastRequest struct {
	Sender         string
	Body           []byte
	IdempotencyKey string
	SignedMint
}

// SendRequest is one directed send.
type SendRequest struct {
	Sender         string
	To             string
	Body           []byte
	IdempotencyKey string
	SignedMint
}

// Broadcast durably records a message addressed to the whole bus and wakes
// every eligible waiter. It returns only once the message is committed and
// fsynced (invariant 4).
func (h *Hub) Broadcast(req BroadcastRequest) (Result, error) {
	return h.publish(publishRequest{
		sender:     req.Sender,
		broadcast:  true,
		body:       req.Body,
		key:        req.IdempotencyKey,
		signedMint: req.SignedMint,
	})
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
	return h.publish(publishRequest{
		sender:     req.Sender,
		broadcast:  false,
		recipients: []string{req.To},
		body:       req.Body,
		key:        req.IdempotencyKey,
		signedMint: req.SignedMint,
	})
}

// publishRequest is the union of everything publish needs. It is a struct
// rather than a parameter list because the list reached nine values with three
// strings and two byte slices adjacent to each other, and a transposed pair
// there — body and signature, sender and message id — would compile and would
// be caught only by a signature that never verifies on some other machine.
type publishRequest struct {
	sender     string
	broadcast  bool
	recipients []string
	body       []byte
	key        string
	signedMint SignedMint
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
//  2. CONSUME the reservation minted for this key (server-authoritative,
//     invariant 1 — see Hub.Mint; the sequence was allocated and DURABLY BURNED
//     at mint time, so nothing is allocated here)
//  3. WRITE THROUGH THE TWO-PHASE PATH AND FSYNC (invariant 4)
//  4. only then apply to the serving copy (invariant 5: disk is the truth)
//  5. only then remember the key and wake waiters (POLL-2)
//
// Step 5 after step 3 is the subtle one and is why POLL-2 exists as its own
// task: a waiter woken before the commit is durable can observe a message that
// a crash then un-observes, which is an acknowledged-but-lost message wearing a
// different hat.
//
// Step 1 BEFORE step 2 is the subtle one SIGN-6 added, and it is what makes the
// in-memory mint table safe: a legitimate retry is answered from the applied-key
// table and never reaches the mint lookup at all, so the fact that the
// reservation was consumed — or lost to a restart — is invisible to it.
func (h *Hub) publish(req publishRequest) (Result, error) {
	sender, broadcast, recipients := req.sender, req.broadcast, req.recipients
	body, key := req.body, req.key

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

	// ADMISSION. Admit expires first, so a table of keys already past the
	// retention window does not refuse a send that has room, and it applies BOTH
	// bounds: the bus-wide cap and this sender's per-agent FAIR SHARE of it
	// (IDEM-11-FU-FAIRSHARE — one agent must never be able to fill the table and
	// deny every other agent its own first applied key).
	//
	// It used to be checked before the sequence was minted, so a refused send
	// burned no sequence. Since SIGN-1's reserve-then-send the sequence is
	// already burned before this function is entered, so the ordering no longer
	// protects the id space — what it protects now is the RESERVATION: the mint
	// below is not consumed until the message is durable, so a send refused here
	// leaves the client holding a still-valid mint and free to retry with the
	// SAME signature once the pressure passes.
	if err := h.idem.Admit(sc); err != nil {
		// The per-agent case is checked FIRST because it is the more specific
		// one: an idem fair-share refusal deliberately satisfies BOTH sentinels
		// (see idem.ErrAgentQuota), so testing ErrCapacity first would report
		// "the bus is full" about a table that is not full and mislead the
		// operator into looking at the bus instead of at one client.
		if errors.Is(err, idem.ErrAgentQuota) {
			return Result{}, newAgentQuotaError("agent %q is at its per-agent share of the applied-key table, which the bus is holding in reserve so no other agent is starved of its own first key; nothing is evicted to make room, because evicting a key turns the next retry of it into a second message: %s", sender, err)
		}
		return Result{}, fmt.Errorf("%w: %d idempotency keys are remembered, the limit; nothing is evicted, because evicting a key turns the next retry of it into a second message", ErrCapacity, h.idemMaxEntries)
	}

	// CONSUME THE RESERVATION. Nothing is allocated here: the sequence was
	// allocated, and DURABLY BURNED, by Hub.Mint (invariant 1 — the server is
	// authoritative on every id, and SIGN-1 chose to hand that id out early so
	// the SENDER can sign it).
	//
	// The mint is LOOKED UP but not yet deleted. It is spent only once the
	// message it names is durable, further down: a send refused between here and
	// the write — at admission, at encoding, at the durable write itself — must
	// leave the client holding a reservation it can retry with the SAME
	// signature, because re-minting would give it a different id to sign and the
	// signature it already computed would be worthless.
	// EXPIRE FIRST, and only here on this path — after the retry check above, so
	// a legitimate retry answered from the applied-key table never depends on the
	// TTL, and before the lookup below, so an expired reservation is NOT
	// spendable.
	//
	// This call was MISSING until 2026-08-07 while mint.go claimed expiry "runs on
	// EVERY mint and EVERY send", which made MintTTL a promise the bus did not
	// keep: an hour-old reservation still spent fine, Mint.ExpiresAt was returned
	// to clients as a fact and was not one, and a hoarding agent's slots were
	// released only if it came back to MINT again — never by sending. The comment
	// was fixed to match the code rather than the other way round in an earlier
	// draft; that was the wrong direction. Honouring a reservation past the expiry
	// this bus PUBLISHED is not "being generous", it is making a documented bound
	// unobservable and untestable, and a bound nobody can observe is one that
	// silently stops holding.
	//
	// Expiring here costs a client nothing it was promised: the answer is
	// ErrUnknownMint, which is documented as ROUTINE and whose remedy — re-mint
	// under the SAME key, re-sign, re-send — cannot double-apply (see that
	// sentinel). The number stays burned either way.
	h.expireMintsLocked(h.now())

	mk := mintKey{agent: sender, op: op, key: key}
	mint, ok := h.mints[mk]
	if !ok {
		// Routine, not a fault: a restart or an expiry. See ErrUnknownMint for
		// why re-minting under the same key is safe and cannot double-apply.
		return Result{}, fmt.Errorf("%w: agent %q has no reservation for this %s key; re-mint under the same idempotency key, re-sign the fresh assignment and re-send", ErrUnknownMint, sender, op)
	}
	if mint.seq != req.signedMint.Seq || mint.messageID != req.signedMint.MessageID {
		// The client presented an assignment this bus did not give it. The
		// presented values are NOT echoed — they are attacker-choosable strings
		// headed for a log line — and the MINTED ones are, because those are the
		// bus's own and are what the client should have signed.
		return Result{}, fmt.Errorf("%w: agent %q was minted %s (sequence %d) for this %s key", ErrMintMismatch, sender, mint.messageID, mint.seq, op)
	}
	seq := mint.seq

	// The id-authority assertion, re-made on the write path. It was already made
	// at mint time; it is made again here because the two are separated by a
	// network round trip and by a client, and an assertion whose value depends on
	// the code in between staying correct is not worth making. See
	// assertSeqFloorLocked, and mint.go for the argument this replaced.
	//
	// It runs BEFORE the durable write, which the OLD check could not do — that
	// one compared the sequence against the WAL index of its own prepare and so
	// could only fire once the message was already on disk and could not be
	// unwritten. This one needs nothing from the write, so a violation costs
	// NOTHING durable.
	if err := h.assertSeqFloorLocked(string(op), mint.messageID, seq); err != nil {
		return Result{}, err
	}

	m, err := store.NewMessage(h.busID, sender, broadcast, recipients, seq, h.now().UTC(), body, key, req.signedMint.TimestampUnixMilli, req.signedMint.Signature)
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
	//
	// The returned wal.Committed is DISCARDED, and that is a change from before
	// SIGN-6: its PrepareIndex was the input to the poison check documented
	// below, and with that check retired there is no longer anything on this path
	// that may be decided by a WAL index. Discarding it explicitly, rather than
	// binding it to a name nothing reads, is the point — a live `committed` here
	// is an invitation to reintroduce an index-versus-sequence comparison that is
	// no longer sound.
	if _, err := h.durable.Write(wal.Entry{
		Kind:  store.RecordKind,
		Body:  payload,
		Idem:  encodedIdem,
		Audit: &wal.AuditRecord{},
	}); err != nil {
		return Result{}, fmt.Errorf("hub: durably recording message %s: %w", m.ID, err)
	}

	// THE RESERVATION IS SPENT, and only now. The message is durable, so the
	// number is unambiguously consumed and no retry may be answered from the
	// mint table again; a retry from here on is answered from the applied-key
	// table, which the durable write above just made recoverable.
	//
	// Everything below this line is already-durable state being reflected into
	// memory, and every failure below POISONS rather than returning, so there is
	// no path on which the mint is deleted for a message that did not land.
	delete(h.mints, mk)
	h.decMintCountLocked(sender)

	// # WHERE THE OLD POISON CHECK WENT — read this before adding one back here
	//
	// This is where `if committed.PrepareIndex < seq { poison }` used to live. It
	// asserted the COUNTING argument Open once rested on: "every sequence issued
	// is <= the WAL index of the prepare carrying it", true while each message
	// consumed one sequence and at least two indices.
	//
	// That argument is RETIRED, and the check with it, because SIGN-1's
	// reserve-then-send makes it FALSE in normal operation: the first mint writes
	// a floor record burning sequences 1..256 while sitting at WAL index 1, so a
	// perfectly healthy bus reaches this line with a sequence far above its
	// prepare index. A check that fires on healthy traffic is worse than no
	// check — a false poison stops the bus for ever, and the fix is invariably to
	// delete the check rather than to understand it.
	//
	// It is replaced by the DIRECT assertion — every sequence handed out is <=
	// the durably-recorded floor — which is strictly stronger (it does not care
	// how many indices a message costs) and which is made ABOVE, before the
	// durable write, and again at the moment the number is issued. Do not
	// reintroduce an index-versus-sequence comparison here: the two counters are
	// no longer related, and wal.Recovered.NextIndex is documented as a distinct
	// counter for exactly this reason.

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
		// It cannot happen — h.idem.Admit was checked under this same lock, and
		// it applies the identical predicate Remember does (the bus-wide cap AND
		// this sender's fair share), while Encode already validated the record —
		// but "cannot happen" is precisely the class of failure that must not be
		// allowed to corrupt the applied-key table silently.
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
