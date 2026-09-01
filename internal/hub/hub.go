package hub

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	// internal/ack is imported for its SENTINEL ERRORS only — see
	// recordAcceptance, which must be able to tell a refusal the recorder has
	// already logged and counted from one it has not. The seam stays an
	// INTERFACE (hub.AckRecorder) rather than the concrete type, so a nil
	// recorder is still the default and a test double is still possible; this
	// import adds no runtime coupling. internal/ack does not import this
	// package, so the direction is safe.
	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/attest"
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

	// MaxWaitersPerAgent bounds how many /v1/wait long polls ONE agent may have
	// parked at once. It is 1: message delivery is SINGLE-ACTIVE per agent id.
	//
	// The bound was 32 until POLL-CONCURRENT-WAITERS. Allowing several parked
	// polls for one identity meant notify SPLIT delivery among them
	// non-deterministically — a DM meant for an interactive session could be
	// woken on a background monitor polling the SAME id instead, and the
	// interactive session would never see it. A live sec-tester reported exactly
	// that. Making the poll single-active removes the split: the FIRST poll
	// holds the slot and a SECOND concurrent poll for the same id is REFUSED
	// (hub.ErrPollActive, HTTP 409), not parked. See the check in Wait for why
	// the bound is per-agent, why an agent-id key is safe on this authenticated
	// route, and why the refusal is a clean response rather than a disconnect
	// (invariant 10).
	//
	// It is NOT a lifetime quota: the slot is released on every exit path of the
	// holding poll (return, timeout, cancel, panic), so the same agent can poll
	// again the instant its previous poll returns.
	MaxWaitersPerAgent = 1
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

// Egress hands a message this bus has just made durable to the cross-bus
// forwarder (RELAY-24-BLOCKER-EGRESS).
//
// It is OPTIONAL. Nil means this bus is not federated and behaves EXACTLY as it
// did before this seam existed — the same equivalence RemoteRouter states about
// itself, and for the same reason: a seam lands before anything is wired to it,
// and the non-federated bus must not be able to tell the difference.
//
// # IT MUST NOT BLOCK, AND IT MUST NOT FAIL A SEND
//
// Forward is called on the send path with writeMu held, AFTER the message is
// durable and after local readers have been woken. It has no return value on
// purpose: there is no outcome it could report that publish would be allowed to
// act on. The local send was already acknowledged by its OWN durable write
// (invariant 4), so a peer's queue, a peer's health and a peer's absence are all
// separate, best-effort-plus-outbox concerns that may never gate or fail the ack.
// relay.Forwarder.Enqueue satisfies this structurally: every queue send there is
// a non-blocking select with a default arm, so a slow or dead peer cannot make a
// local send slow.
//
// An implementation MUST be safe for concurrent use.
type Egress interface {
	// Forward offers m to the cross-bus egress path. It must return promptly and
	// must not panic; the hub guards against both, but a guard is a backstop and
	// not a licence.
	Forward(m store.Message)
}

// AckRecorder records DURABLE LOCAL ACCEPTANCE — one sender-visible delivery
// lifecycle row per recipient — for a message this bus has just committed
// (ACK-2; ACK-CONTRACT.md §7, §8.2 event E1).
//
// It is OPTIONAL. Nil means no lifecycle table is wired and this bus behaves
// EXACTLY as it did before the seam existed — the same equivalence RemoteRouter
// and Egress state about themselves.
//
// # IT IS THE *SENDER-VISIBLE* PLANE, WHICH IS NOT THE HOP PLANE
//
// Three facts are routinely collapsed into the word "ack": this bus committed
// the message (plane A); the next bus took responsibility for a copy (plane B);
// the addressed agent's application accepted it (plane C). This seam records
// plane A. Plane B is relay.Outbox's and is a DIFFERENT table on purpose — a hop
// ACK must never advance the state this records, which is why there is no method
// here for one.
//
// # IT MUST NOT FAIL A SEND, AND publish MUST NOT LET IT
//
// Accept is called on the send path AFTER the message's own two-phase write has
// returned, so the message is durable and the sender is already owed its 201.
// An error from it — including the capacity refusals the lifecycle table is
// designed to return — DEGRADES THE OBSERVATION AND NEVER THE SEND
// (ACK-CONTRACT.md §11.3): refusing here would mean an observability table
// causing a messaging outage, and it would break everything while violating
// nothing, since the message is already on stable storage. publish logs the
// refusal loudly (invariant 6) and carries on.
//
// It DOES perform a durable write of its own, so it is not free: a send with a
// recorder wired costs one additional two-phase fsync cycle PER RECIPIENT — which
// is one today, because SendRequest carries a single `To`. That cost
// is the price of invariant 4 holding for the lifecycle row as well — a row that
// is only in memory would report outcomes no restart could reproduce.
//
// An implementation MUST be safe for concurrent use.
type AckRecorder interface {
	// Accept records that this bus has committed and fsynced the message
	// identified by correlationKey — the ORIGIN bus's server-minted message id,
	// which for a locally-originated message is its own id — addressed to
	// recipient, on behalf of sender. Both agent ids are fully qualified
	// (invariant 2).
	//
	// A repeat call for the same (correlationKey, recipient) is a legitimate
	// retry and must be a no-op returning nil (invariant 10).
	Accept(correlationKey, sender, recipient string) error
}

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

	// RemoteRouter admits recipients this bus does NOT hold but that a peer bus
	// does (RELAY-16). It is OPTIONAL and nil is the correct value for a bus that
	// is not federated.
	//
	// Nil is not a degraded mode and has NO default: with no router every
	// recipient absent from Roster is refused with ErrUnknownRecipient, which is
	// precisely what this bus did before the seam existed. Contrast Roster, whose
	// absence is a startup ERROR — an empty roster is silently catastrophic
	// because it makes the bus serve nobody, whereas an absent router only
	// declines to widen admission, which is the safe direction.
	//
	// It never overrides the roster: see RemoteRouter and Hub.routeRemote for why
	// an id in THIS bus's namespace is never offered to it.
	RemoteRouter RemoteRouter

	// Egress is the OPTIONAL cross-bus forwarding seam (see the Egress type).
	// Nil is the correct value for a bus that is not federated, and is
	// behaviourally identical to the bus before the seam existed.
	//
	// It is the PEER of RemoteRouter and the two belong to one another: the
	// router admits a recipient behind a peer bus, this carries the message
	// there. RemoteRouter's own "DO NOT INJECT A ROUTER EARLY" note is the
	// binding half of that pairing — a router wired without an Egress accepts
	// messages it has no way to deliver, which is worse than the honest 404 it
	// replaced.
	Egress Egress

	// Acks is the OPTIONAL durable sender-visible delivery lifecycle table (see
	// the AckRecorder type). Nil is the correct value for a build with no
	// lifecycle table, and is behaviourally identical to the bus before the seam
	// existed.
	Acks AckRecorder

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

	// LogRepaired, when non-empty, says that recovery PHYSICALLY REMOVED
	// records from the durable log before this replay ran, phrased for an
	// operator (for example: "the tail was truncated, removing 4096 bytes").
	// It is "" on the ordinary start, where the log was read exactly as it was
	// written.
	//
	// # What it is FOR, and why NextIndex does not already cover it
	//
	// It is the predicate for the one case where a MISSING message-seq-floor
	// file is unsafe. All three log-derived floor sources — the high-water
	// index, the replayed "seqfloor" records and the highest replayed message
	// sequence — assume the log is COMPLETE. Truncate it, rewrite its middle or
	// quarantine it and every one of them silently drops to whatever survived,
	// while the numbers already handed out by /v1/mint did not.
	//
	// NextIndex cannot stand in for this. Since the mint burns numbers in
	// batches of MintBatchSize, 300 sequences can be carried by a handful of
	// records, so the index high-water mark is no upper bound on the sequence
	// at all: measured, a bus that had handed out 300 sequences resumed at 25
	// after this exact damage.
	//
	// # What it does NOT cover — it is a PROXY, not the question
	//
	// It answers "did recovery physically remove records", which is NOT the same
	// question as "is the log complete". The two differ when the log was cut
	// EXACTLY ON A RECORD BOUNDARY: replay reads to EOF, finds nothing torn,
	// discards nothing, and this field is "" — while the records past the cut
	// are gone, removed by the cut rather than by recovery.
	//
	// That blindness is TOTAL, not statistical: on an exhaustively swept real
	// specimen this field was "" at 22 of 22 record boundaries — 100%, as it must
	// be, since nothing is torn there — and 13 of those 22 starts derived a floor
	// BELOW a sequence already delivered, one reissuing a message id end to end.
	// The other nine were harmless only because that directory's floor had
	// already stepped past its delivered high-water, so the harmful fraction is a
	// property of a DIRECTORY'S HISTORY, not of the defect. Never restate this as
	// a percentage of byte offsets (it reads as 0.29% and invites dismissal).
	//
	// And the floor itself is not the blind half: a paired test on an unguarded
	// build derived IDENTICAL floors for boundary-exact and mid-record cuts at
	// the same offset, so derivation is one monotonic step function whose steps
	// land on record boundaries and the two recovery paths cannot disagree about
	// the floor. The only thing that differs is whether this field is set — which
	// is exactly why it is the wrong question. (All 22 were clean tail repairs:
	// quarantined_to= absent on every one, verified directly.)
	//
	// Closing that needs the consumed-index field (Spec Server task 9fd58deb),
	// not a different reading of this one. Anything relying on this field as
	// proof of completeness must say so no more strongly than the field can
	// support; see the migration WARN in Open.
	//
	// The CALLER decides what counts as removal, because the caller is the one
	// holding wal.Recovered. Today cmd/agent-bus sets it for a quarantine, a
	// truncation, a mid-file rewrite, and bytes discarded at the framing stage.
	// It deliberately does NOT set it for a dangling prepare or for holes in
	// the index sequence: those are the ordinary signature of a clean crash and
	// of index ranges a reservation burned but no record ever used, they remove
	// nothing from the file, and treating them as loss would refuse to start
	// every legacy data directory that had ever crashed.
	LogRepaired string

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
	// The roster is validated BEFORE writeMu is taken — publish checks the
	// sender and the recipients first — and that remains the position on the
	// ADMISSION path. It is why Enrolled's TOCTOU note matters, so read that
	// before moving the roster check inside the lock to "tidy this up".
	//
	// # THE EGRESS SEAM DOES READ A ROSTER UNDER writeMu (2026-08-15)
	//
	// An earlier version of this note said "nothing here may hold writeMu across
	// a call into the roster", and forwardOnward now does exactly that: it is
	// called at the end of publish, with writeMu held, and the injected
	// hub.Egress implementation (cmd/agent-bus's relayEgress) reads the
	// enrolment roster to find the sender's messaging public key. On the shipped
	// wiring that is auth.WALRoster.Get, which takes an EXCLUSIVE sync.Mutex —
	// the same object this package sees through the injected RosterSource. The
	// prohibition was silently false, and a false comment about lock order is
	// how the next deadlock gets written, so it is corrected rather than left.
	//
	// WHY IT IS SAFE, which is a different claim from "it does not happen":
	// writeMu -> roster is now a real edge, and it is safe only while there is no
	// edge back. There is not. The roster's own lock is held ONLY across its own
	// map operations; auth.WALRoster's durable writes go to the wal.Log, which
	// knows nothing about the hub, and no roster path calls into this package at
	// all. So the order is total — writeMu -> {waitMu, store lock, roster lock} —
	// with nothing taking writeMu while holding any of the three.
	//
	// THE RULE THAT REPLACES THE OLD ONE: an Egress implementation may take a
	// lock that is a LEAF with respect to the hub, and may not take one that
	// anything reachable from the hub's own lock could be waiting on. It must
	// also not block — see forwardOnward, which states what "does not block"
	// does and does not cover.
	//
	// The read paths (History, Wait, Agents) never take writeMu at all, which is
	// what keeps a long poll off the fsync path.
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

	// router is the OPTIONAL egress-admission seam (RELAY-16). Nil on a
	// non-federated bus, and nil is behaviourally identical to the bus before the
	// seam existed. Set once in Open, never replaced; it is consulted only
	// through routeRemote, which is where the invariants on its answer live.
	router RemoteRouter

	// egress is the OPTIONAL cross-bus forwarding seam. Nil on a non-federated
	// bus. Set once in Open, never replaced; it is called only through
	// forwardOnward, which is where the guarantees about it live.
	egress Egress

	// acks is the OPTIONAL durable sender-visible delivery lifecycle table. Nil
	// on a build with none. Set once in Open, never replaced; it is called only
	// through recordAcceptance, which is where the guarantees about it live.
	acks AckRecorder

	// recovered holds every agent id named as a sender or a recipient by a
	// message replayed from disk at startup. It is written only during Open and
	// read-only afterwards, so it needs no lock.
	//
	// It exists to make ONE thing observable: a LOCALLY-QUALIFIED id that appears
	// here and is NOT in the roster is an id whose holder is gone and whose name
	// could be minted again, which invariant 1 forbids. Since RELAY-16 a durable
	// record may also name a FOREIGN recipient, which this bus's roster was never
	// going to hold and which says nothing about this bus's id space; the
	// consumer filters those out rather than the harvest, so this map stays the
	// raw fact about what was written. See noteRecoveredIdentities.
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
		router:         o.RemoteRouter,
		egress:         o.Egress,
		acks:           o.Acks,
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
	var err error
	h.idem, err = idem.NewStoreForBus(o.BusID, idem.StoreOptions{Now: h.now, MaxEntries: o.MaxIdempotencyEntries})
	if err != nil {
		return nil, fmt.Errorf("hub: open idempotency store: %w", err)
	}
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
	// THE GUARD ON THE MIGRATION PATH. It must sit HERE — after every log-derived
	// source has been folded in, and before the derived floor is persisted and
	// sealed — because it is a statement about the floor that was just derived:
	// that it was derived from a log this same start has already proven
	// incomplete.
	//
	// # The case, and the measurement
	//
	// A missing floor file is a SUPPORTED UPGRADE PATH: a data directory written
	// by an agent-bus that predates the file has none, and rebuilding one from
	// the log is exactly right — WHEN THE LOG IS INTACT. Combine it with a
	// damaged log and the fallback becomes a fabrication. Measured on a real
	// directory: 300 sequences minted, handed out and signable; delete the floor
	// file, truncate the log; the bus starts happily and mints 25, walking back
	// up through 275 numbers a client may hold a signature over.
	//
	// # Why the harm is invisible rather than noisy
	//
	// The reissued number produces a SECOND message under an existing message
	// id, with different content. Our own documentation requires consumers to
	// deduplicate on message id — so a CORRECTLY IMPLEMENTED consumer sees the
	// repeat, concludes it is the duplicate it was told to expect, and DROPS the
	// new message. The more correct the client, the more reliably it loses data,
	// and neither end sees anything wrong.
	//
	// # Why refusing, and not deriving something clever
	//
	// This is the same answer the CORRUPT path already gives, and that path's
	// error already states this precondition in as many words: the log fallback
	// is "correct ONLY if that log has not also been damaged or quarantined".
	// The corrupt path refused and explained itself; this path performed the
	// identical unsafe fallback silently and then logged that it had "closed the
	// window". Only the guard was missing — the knowledge was already written
	// down, in openSeqFloorFile's own comment naming "missing-file plus
	// quarantine on the SAME start" as the one uncovered case.
	//
	// There is no sound derivation to fall back to. Nothing left on disk knows
	// what /v1/mint handed out, which is the entire reason the floor file exists
	// outside the log. Resuming from a guess would be inventing an id-authority
	// claim, and invariant 1 is not a thing to be approximated.
	//
	// # Availability, honestly
	//
	// This can only fire when the floor file is ABSENT, so the population is
	// legacy data directories and directories where someone removed the file.
	// One clean start writes the file and the guard can never fire again. A
	// genuinely fresh directory has no log to damage, so it cannot trip. The
	// cost is therefore a one-time refusal on a legacy directory whose log was
	// ALSO damaged — and on that directory the alternative is not availability,
	// it is silent id reuse.
	if h.seqFloorFile != nil && !h.seqFloorFile.existedAtOpen() && o.LogRepaired != "" {
		return nil, fmt.Errorf("%w: %s does not exist AND the durable log %s, so the sequence high-water mark cannot be recovered from either source. "+
			"The floor file is the only record of numbers handed out by /v1/mint and never written to the log; without it the floor falls back to what the log proves, and this start has just proven the log incomplete. "+
			"Starting would resume the message sequence at %d, reissue every number above that which was already handed to a client and possibly SIGNED, and produce two different messages under one message id (invariant 1) — which a correctly-implemented consumer deduplicating on message id will resolve by silently DROPPING the new one. "+
			"This is the case openSeqFloorFile documents as uncovered: a data directory that predates %s, restarted after damage to its log. "+
			"DO NOT SIMPLY RESTART: this refusal is a property of the data directory, not a transient failure, so restarting reaches this same point — and if your supervisor restarts the bus automatically (docker-compose ships `restart: unless-stopped`), it is doing exactly that on your behalf. Stop the supervisor before working through the remedies below, or you will be repairing a directory that is being restarted underneath you. "+
			"Remedies, in order of preference: restore %s from a backup taken before the damage and restart; or, if you know a value at or above the highest sequence this bus ever handed out, write it to %s yourself — the format is two plain-text lines, %q followed by \"floor <n>\", where the digest is an unkeyed SHA-256 over the second line (a floor that is too HIGH is safe, it only skips numbers; too low is the reuse this refuses); or, if this directory has no history worth preserving, move it aside and start a fresh one",
			ErrSeqFloorUnprovable, h.seqFloorFile.Path(), o.LogRepaired, floor+1, SeqFloorFileName,
			h.seqFloorFile.Path(), h.seqFloorFile.Path(), seqFloorFileMagic+" v"+strconv.Itoa(seqFloorFileVersion)+" sha256=<hex>")
	}

	if h.seqFloorFile != nil {
		migrating := !h.seqFloorFile.existedAtOpen() && floor > 0
		if err := h.seqFloorFile.raise(floor); err != nil {
			return nil, fmt.Errorf("hub: recording the durable message sequence floor before serving: %w", err)
		}
		if migrating {
			// The MIGRATION window, named. A data directory with history but no
			// floor file was written by a binary that predates the file, and until
			// this write landed a quarantine could have reissued a minted
			// sequence. It is a WARN and not an ERROR because a floor was
			// derivable and the bus can serve; it is NOT a WARN because the
			// window is closed. Read the next paragraph before touching either
			// this wording or the predicate above.
			//
			// # WHAT THE GUARD ABOVE COVERS, AND THE HOLE IT DOES NOT
			//
			// The guard's predicate is o.LogRepaired, which answers "did recovery
			// PHYSICALLY REMOVE records" — a PROXY for "the log is complete", and
			// the proxy has one hole. A truncation landing exactly on a record
			// boundary removes nothing during recovery: replay reads to EOF, finds
			// no torn record, sets LogRepaired to "", and this line is REACHED
			// with records missing. They were removed by the CUT, not by recovery.
			//
			// THE HOLE IS SYSTEMATIC, NOT A CORNER CASE — state it as a fraction
			// of BOUNDARIES, never of offsets. A real specimen was swept
			// exhaustively end to end (8900 offsets, both halves): every non-
			// boundary offset REFUSED, loudly and never silently — that is the
			// part worth keeping — and the guard failed at 22 of 22 RECORD
			// BOUNDARIES, i.e. 100% of them. It must: the guard cannot fire where
			// nothing is torn, so the escape set IS the boundary set. The escapes
			// are 360, 427, 738, 805, 937, 1004, 2016, 2083, 3095, 3162, 4172,
			// 4240, 4372, 4440, 5487, 5555, 6602, 6670, 7717, 7785, 8832, 8900 —
			// spaced alternately ~1047 and ~68, which is message-record plus
			// small-record framing.
			//
			// 13 of the 22 reissued a sequence already delivered (floor 22 at the
			// first five, floor 256 at the next eight, against a highest-delivered
			// of 260); the other nine were harmless only because the floor had
			// already stepped past the delivered high-water by that point. So
			// 13/22 is a fact about ONE SPECIMEN'S HISTORY — a directory with more
			// traffic after its last floor step has a higher harmful fraction —
			// while 22/22 is the fact about this code. Do not re-derive a
			// percentage over offsets (it reads as 0.29% and invites closing this
			// as rare).
			//
			// Single-byte sharp and reproducible 3/3 (2015 refuses, 2016 starts,
			// 2017 refuses). End to end at offset 2016 message id
			// bus-7ubqgqor3zshldk3-257 was issued TWICE with different content
			// hashes — invariant 1, broken, on a start this line used to call
			// closed.
			//
			// And the floor derivation is NOT the blind half. On an unguarded
			// build, boundary-exact and mid-record cuts at the SAME offset derived
			// identical floors (checked at 1004, 2016, 4240, 4440, 5487, 6602,
			// 7785): derivation is one monotonic step function whose steps land on
			// record boundaries, so the two recovery paths cannot disagree about
			// the floor. The ONLY thing that differs is whether LogRepaired is
			// set — which is exactly why the guard is blind while the floor is not.
			//
			// And these are clean TAIL REPAIRS, not quarantine artefacts: the
			// whole-file quarantine branch was ruled out directly, quarantined_to=
			// absent on all 22. (An earlier reading had this as confirmed only by
			// consequence — the step function would have collapsed to the
			// file-only source under a quarantine — because the sweep's first
			// discriminator matched prose. It was then checked properly.)
			//
			// SO THE MESSAGE BELOW CLAIMS ONLY THE DISCARD CHECK. It previously
			// said this start "verified that recovery removed no records ... so
			// this start closes the window", which is literally true (recovery
			// discarded nothing) and substantively false (records are gone). The
			// guard and its own reassurance share ONE blind spot, so the operator
			// was told the window was closed at precisely the offsets where it was
			// open — a confident false all-clear, which is worse than silence
			// because it spends the operator's uncertainty in the wrong direction.
			// Do not restore that claim: TestSeqFloorMigrationWarningDoesNotClaimTheLogIsComplete
			// pins its absence.
			//
			// Closing the hole for real needs a check that does not go through
			// LogRepaired at all — the consumed-index field, tracked as Spec
			// Server task 9fd58deb. Until that lands, the predicate above stays as
			// it is (every non-boundary cut is still refused loudly, which is
			// worth keeping) and the honesty lives in the wording.
			//
			// Keep the message under logging's 1024-byte msg cap (maxValueLen; it
			// currently sits at 993), or the caveat — which is at the END — is
			// exactly the half that gets truncated away, leaving the reassuring
			// half behind. An earlier draft of this very message overran the cap
			// by 39 bytes and the test below caught it; it asserts no truncation
			// for that reason, so trim before you add.
			//
			// The "UNPROVEN FLOOR:" prefix is there for the same reason and covers
			// what the cap check cannot: truncation downstream of us — a 1024-octet
			// RFC 3164 syslog hop, a shipper, an operator's `cut` — also drops the
			// TAIL, so with the qualifier only at the end every such path keeps the
			// reassuring half and loses the caveat. Front-loading the verdict means
			// the first thing surviving any truncation is the word that matters.
			h.log.Warn("UNPROVEN FLOOR: this data directory had no durable message sequence floor file: it was written by an agent-bus that predates it. Until now a WAL quarantine could have reissued numbers already handed out by /v1/mint. The file has been created from the floor the durable log proves, and recovery discarded no records from that log on this start. That is the ONLY check that ran, and it does NOT establish that the log is complete: ANY truncation landing exactly on a record boundary leaves nothing torn for recovery to discard, so this check is blind at EVERY record boundary, and a floor derived from a log cut that way can sit BELOW numbers /v1/mint has already handed out. Treat the floor as UNPROVEN: if this log may ever have been truncated or partially restored, stop the bus, compare the floor against the highest sequence you know it issued and raise the file by hand before restarting (it is digest-protected; see CONTRACTS-ONDISK.md. Too high only skips numbers, too low reissues them)",
				"seq_floor_file", h.seqFloorFile.Path(),
				"floor", floor,
			)
		}
		// And make the file exist even when the floor is 0, which raise()
		// deliberately will not do. This is what makes "the file is absent"
		// mean exactly one thing — a data directory older than the file —
		// instead of also covering a fresh directory that has never minted.
		// The guard above depends on that distinction; see ensureExists.
		if err := h.seqFloorFile.ensureExists(); err != nil {
			return nil, fmt.Errorf("hub: creating the durable message sequence floor before serving: %w", err)
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
	// THE DELIVERY POSITION, from the SAME field the live path uses
	// (SIGN-1-FU-REORDER-WATERMARK). Replay folds the log in commit order, so
	// stamping the commit index here reproduces exactly the positions the
	// messages had before the restart — which is what makes a client's stored
	// cursor still mean what it meant, rather than pointing into a renumbered
	// window.
	m.Pos = c.CommitIndex
	if err := h.store.Append(m); err != nil {
		h.log.Error("DISCARDING a message record that could not be applied during recovery; it is not in this bus's history and will not be delivered",
			"prepare_index", c.PrepareIndex,
			"message_id", m.ID,
			"seq", m.Seq,
			"pos", m.Pos,
			"err", err,
		)
		return nil
	}
	// appliedSeq tracks the SEQUENCE, not the position, and the two must not be
	// mixed: it feeds the mint sequence floor (invariant 1), which is a statement
	// about numbers this bus has handed out to be signed, not about where records
	// sit in the log.
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
	// The idempotency OUTCOME is deliberately dropped here and NOT folded into
	// Result. This route reports a retry through Result.Replayed, as it always
	// has, and a violation through ErrIdempotencyKeyReused; the three-way split
	// is carried uncollapsed only where a caller needs all three, which is the
	// relay ingest (see IngestRelayed).
	res, _, err := h.publish(publishRequest{
		sender:     req.Sender,
		broadcast:  true,
		body:       req.Body,
		key:        req.IdempotencyKey,
		signedMint: req.SignedMint,
	})
	return res, err
}

// Send durably records a message addressed to one agent and wakes that agent's
// waiters. It returns only once the message is committed and fsynced
// (invariant 4).
//
// The recipient must be addressable: either enrolled on THIS bus, or — since
// RELAY-16 — a foreign id that the configured RemoteRouter says a peer bus holds.
// Anything else is ErrUnknownRecipient and nothing is written. A message accepted
// for a remote recipient is durable on this bus before this returns, exactly like
// a local one (invariant 4); handing it onward is a later, separate step and is
// never a substitute for the write.
func (h *Hub) Send(req SendRequest) (Result, error) {
	if _, _, _, err := ids.ParseAgentID(req.To); err != nil {
		return Result{}, fmt.Errorf("%w: %s", ErrInvalidRecipient, err)
	}
	// See Broadcast for why the outcome is dropped on this route.
	res, _, err := h.publish(publishRequest{
		sender:     req.Sender,
		broadcast:  false,
		recipients: []string{req.To},
		body:       req.Body,
		key:        req.IdempotencyKey,
		signedMint: req.SignedMint,
	})
	return res, err
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

	// busPath is the ordered list of buses this message has already traversed,
	// ending with THIS bus, and it is set ONLY for a message INGESTED FROM A
	// PEER (RELAY-11).
	//
	// EMPTY MEANS LOCAL, and that is the default on purpose: Send and Broadcast
	// leave it unset and publish substitutes store.LocalBusPath(h.busID), so a
	// send from one of this bus's own clients records exactly the single hop it
	// recorded before a path could be supplied at all. The audit trail's
	// continuity depends on that default being the same value, not merely a
	// similar one.
	//
	// IT IS NEVER CLIENT INPUT. There is no field on SendRequest or
	// BroadcastRequest that reaches here, and there must not be one: a client
	// that could choose its own bus path could forge the provenance of a message
	// in an append-only trail, claiming it originated on a bus it has never
	// spoken to. The only legitimate source is the relay ingest path, which
	// derives it from the authenticated peer connection and appends this bus
	// itself. store.NewMessageWithBusPath re-validates every hop regardless.
	busPath []string

	// relayed marks a message INGESTED FROM A PEER BUS rather than sent by one
	// of this bus's own clients. It is set ONLY by IngestRelayed, which is the
	// one exported caller that may set it, and it changes exactly four things —
	// each of them a rule that is WRONG for a relayed message rather than merely
	// inconvenient:
	//
	//  1. the SENDER is required NOT to be ours (checkRelayedSender) instead of
	//     being required to be on our roster. A relayed message's sender belongs
	//     to the ORIGIN bus by construction (invariant 2), so the local roster
	//     check would refuse every legitimate relay.
	//  2. the RECIPIENT rule becomes checkRelayedRecipient: an id claiming OUR
	//     namespace must be on our roster, and a foreign one is recorded without
	//     asking whether anyone can route it — a message with no onward route is
	//     still durably ours.
	//  3. the operation is idem.OpRelay, so a relayed message can never share an
	//     applied-key scope, or a fingerprint, with a local send.
	//  4. the sequence is minted INTERNALLY (mintRelayedSeqLocked) rather than
	//     consumed from a client reservation. A peer bus holds no mint on this
	//     bus and can never obtain one.
	//
	// It changes NOTHING about the durability, the ordering or the idempotency
	// of the write: there is one two-phase write path and this is not a second
	// one (invariant 4).
	relayed bool

	// originMessageID and originSeq are the ORIGIN bus's assignment for a
	// relayed message: the id its own bus minted, and the sequence half of it.
	// Set only when relayed is set, and validated by IngestRelayed before it
	// gets here.
	//
	// They are NOT this message's identity — the local record's id is the one
	// this bus minted (invariant 1). They exist for ONE thing: the audit
	// record's content hash, which must cover the bytes the stored signature
	// actually covers. See signedAs in audit.go, which is the whole argument.
	originMessageID string
	originSeq       uint64

	// originAttestation is the ORIGIN bus's signed binding for a relayed
	// message's sender, on its way to the DURABLE record (RELAY-48). Set only
	// when relayed is set.
	//
	// It is NOT an audit input and is not a third member of the pair above: the
	// audit content hash is computed over the SIGNING bytes, which do not include
	// it, and wal.AuditRecord never carries it. Its one purpose is that
	// Forwarder.Resume can rebuild an onward envelope after a restart, which it
	// cannot do from anything else this bus holds.
	originAttestation attest.Attestation
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
//
// # THE RELAY INGEST SHARES THIS FUNCTION, and that is a requirement
//
// A message arriving from a peer bus (req.relayed — see IngestRelayed) takes
// EXACTLY this path: the same applied-key adjudication, the same single
// two-phase write, the same wake-up, in the same order under the same lock. A
// second durable write path for relayed traffic would be a second answer to
// "have I applied this?", which is the thing invariants 4 and 10 both forbid;
// what req.relayed changes is documented on the field itself and is confined to
// four clearly-marked branches below.
//
// # THE SECOND RETURN VALUE
//
// The idempotency OUTCOME is returned UNCOLLAPSED — new, retry, violation — and
// it is MEANINGFUL ONLY WHEN THE ERROR IS NIL, with one deliberate exception:
// the violation is reported as idem.OutcomeViolation AND as
// ErrIdempotencyKeyReused, so a caller may classify it either way. On every
// other error path the value is idem.OutcomeNew because that is the zero value
// and there is no "unknown" member to return — check the error FIRST. See
// RelayedIngestResult.Outcome, which restates this for the exported surface.
//
// It is a distinct value rather than a field on Result because Result.Replayed
// is a BOOLEAN and a boolean cannot carry three answers: a caller that
// re-derived the outcome from it would report "new" for a violation, and "new"
// is the answer the relay re-forwards on.
func (h *Hub) publish(req publishRequest) (Result, idem.Outcome, error) {
	sender, broadcast, recipients := req.sender, req.broadcast, req.recipients
	body, key := req.body, req.key

	if err := validateIdempotencyKey(key); err != nil {
		return Result{}, idem.OutcomeNew, err
	}
	if len(body) == 0 {
		return Result{}, idem.OutcomeNew, fmt.Errorf("%w: a message body is required", ErrInvalidBody)
	}
	if len(body) > store.MaxBodyBytes {
		return Result{}, idem.OutcomeNew, fmt.Errorf("%w: %d bytes, the limit is %d", ErrInvalidBody, len(body), store.MaxBodyBytes)
	}
	// THE SENDER GATE, AND ITS INVERSION FOR A RELAYED MESSAGE.
	//
	// A local send's sender must be on THIS bus's roster. A relayed message's
	// sender must NOT be ours — it belongs to the origin bus by construction
	// (invariant 2), so applying the roster rule to it would refuse every
	// legitimate relay, and applying NO rule would let a peer assert an id in our
	// namespace, which is the permanent id-space injury cca64afd names. The two
	// rules are exclusive alternatives, never both and never neither.
	if req.relayed {
		if err := h.checkRelayedSender(sender); err != nil {
			return Result{}, idem.OutcomeNew, err
		}
	} else if !h.Enrolled(sender) {
		return Result{}, idem.OutcomeNew, fmt.Errorf("%w: %q", ErrUnknownSender, sender)
	}
	for _, r := range recipients {
		if req.relayed {
			// A RELAYED MESSAGE'S RECIPIENTS ARE ADJUDICATED DIFFERENTLY, and the
			// roster is still asked FIRST and BEFORE the durable write — see
			// checkRelayedRecipient for the two rules and why an unroutable
			// foreign recipient is accepted here while the local path refuses it.
			if err := h.checkRelayedRecipient(r); err != nil {
				return Result{}, idem.OutcomeNew, err
			}
			continue
		}
		// THE ROSTER IS ASKED FIRST, AND BEFORE THE DURABLE WRITE. Both halves of
		// that sentence are requirements, not style.
		//
		// BEFORE THE WRITE, so a recipient nobody can reach costs nothing durable —
		// and, since cca64afd, because a local id admitted by anything other than
		// the roster is a permanent id-space injury: a relay ingest naming
		// "<local-bus>.alpha-18446744073709551615" would push that name's suffix
		// floor to the top and exhaust "alpha" across every future restart (see
		// cmd/agent-bus/suffixfloors.go). Do not move this below the write.
		//
		// FIRST, so the roster remains the ONLY authority on this bus's own
		// namespace. routeRemote enforces the same thing from its side by refusing
		// to offer a locally-qualified id to the router at all.
		if h.Enrolled(r) {
			continue
		}
		// NOT ON THE ROSTER. Before RELAY-16 that was the end of it. Now a
		// FOREIGN "<bus>.<agent>" may be admissible if a peer bus holds it — and
		// with no router configured, routeRemote returns false for everything, so
		// this reduces to the pre-RELAY-16 refusal exactly.
		if _, ok := h.routeRemote(r); ok {
			continue
		}
		if h.router == nil {
			// Byte-identical to the pre-RELAY-16 message: a bus with no router
			// must not report federation it does not have.
			return Result{}, idem.OutcomeNew, fmt.Errorf("%w: %q is not enrolled on this bus", ErrUnknownRecipient, r)
		}
		// A federated bus can say more, and should: "unknown" here means BOTH
		// authorities were asked and neither holds the recipient. The honest 404
		// is the feature — a routable-looking id that no peer advertises must not
		// be accepted and then silently dropped.
		return Result{}, idem.OutcomeNew, fmt.Errorf("%w: %q is not enrolled on this bus and no peer bus advertises it", ErrUnknownRecipient, r)
	}

	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	if h.poisoned != nil {
		return Result{}, idem.OutcomeNew, h.poisoned
	}
	if h.durable == nil {
		return Result{}, idem.OutcomeNew, ErrNotDurable
	}

	op := idem.OpSend
	switch {
	case req.relayed:
		// idem.OpRelay, and it is DOMAIN SEPARATION rather than labelling: the op
		// is part of the applied-key scope AND of the fingerprint, so a relayed
		// message can never share a scope with — or be mistaken for a retry of —
		// a local send under the same key. It is also the scope
		// relay.RelayedMessage.Scope() names, which is what makes the same message
		// arriving by two disjoint routes resolve to ONE key.
		op = idem.OpRelay
	case broadcast:
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
		return Result{}, idem.OutcomeNew, fmt.Errorf("hub: building the idempotency scope for %s: %w", op, err)
	}
	fp := publishFingerprint(op, recipients, body)
	prev, outcome := h.idem.Lookup(sc, fp)
	switch outcome {
	case idem.OutcomeViolation:
		// Same key, DIFFERENT payload. Not a retry — a protocol violation.
		// NEITHER payload is echoed into the error: the caller already knows
		// what it sent, and the other one belongs to a message it may have no
		// business seeing quoted back.
		return Result{}, idem.OutcomeViolation, fmt.Errorf("%w: key %q was applied for message %s", ErrIdempotencyKeyReused, key, storedMessageID(prev))
	case idem.OutcomeRetry:
		// A legitimate retry: the ack was probably lost in flight. Return the
		// ORIGINAL result, re-apply nothing, error nothing, disconnect nobody.
		out, err := decodeStoredResult(prev)
		if err != nil {
			// The stored result is unreadable, so the original answer cannot be
			// returned. Failing is the only honest option: re-applying would be
			// the double-apply invariant 10 forbids.
			return Result{}, idem.OutcomeNew, fmt.Errorf("hub: replaying the original result for idempotency key %q: %w", key, err)
		}
		out.Replayed = true
		return out, idem.OutcomeRetry, nil
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
			return Result{}, idem.OutcomeNew, newAgentQuotaError("agent %q is at its per-agent share of the applied-key table, which the bus is holding in reserve so no other agent is starved of its own first key; nothing is evicted to make room, because evicting a key turns the next retry of it into a second message: %s", sender, err)
		}
		return Result{}, idem.OutcomeNew, fmt.Errorf("%w: %d idempotency keys are remembered, the limit; nothing is evicted, because evicting a key turns the next retry of it into a second message", ErrCapacity, h.idemMaxEntries)
	}

	// THE SEQUENCE. Two ways in, ONE rule: the server is authoritative on every
	// id and never reuses one, including across restarts (invariant 1).
	//
	// A LOCAL SEND consumes the reservation this bus minted for the key, because
	// SIGN-1 hands the number out early so the sender can SIGN it. A RELAYED
	// MESSAGE has no reservation and can never have one — a peer bus does not
	// enrol here, cannot call Mint, and must not be able to choose a number in
	// our namespace — so this bus mints the number itself, at the moment it takes
	// responsibility for the message. Both routes burn the number durably before
	// it is used; see mintRelayedSeqLocked.
	var (
		seq      uint64
		mintedID string
		mk       mintKey
	)
	if req.relayed {
		// The relayed sequence is allocated HERE, under writeMu, AFTER the
		// applied-key adjudication above — so a duplicate arriving by a second
		// route is answered from the table and burns no number at all — and
		// AFTER admission, so a bus at its bound burns none either.
		//
		// A failure between here and the durable write leaves the number burned
		// and a GAP in the sequence. That is CORRECT and is not damage to repair
		// (internal/ids/sequence.go): a gap is the safe direction, a reissue is
		// not.
		if req.signedMint.MessageID != "" || req.signedMint.Seq != 0 {
			// FAIL CLOSED on a caller that presented a reservation on the one path
			// that has none to spend. SignedMint rides along here for its
			// TIMESTAMP and SIGNATURE only (see IngestRelayed); an id or a sequence
			// in it would be a client-chosen id being carried towards a durable
			// record, which invariant 1 forbids outright. Nothing sets it today —
			// this is the check that keeps that true.
			return Result{}, idem.OutcomeNew, fmt.Errorf("%w: a relayed ingest presented a message id or sequence, but a peer bus holds no reservation on this bus and this bus mints the number itself (invariant 1)", ErrMintMismatch)
		}
		var err error
		if seq, mintedID, err = h.mintRelayedSeqLocked(); err != nil {
			return Result{}, idem.OutcomeNew, err
		}
	} else {
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

		mk = mintKey{agent: sender, op: op, key: key}
		mint, ok := h.mints[mk]
		if !ok {
			// Routine, not a fault: a restart or an expiry. See ErrUnknownMint for
			// why re-minting under the same key is safe and cannot double-apply.
			return Result{}, idem.OutcomeNew, fmt.Errorf("%w: agent %q has no reservation for this %s key; re-mint under the same idempotency key, re-sign the fresh assignment and re-send", ErrUnknownMint, sender, op)
		}
		if mint.seq != req.signedMint.Seq || mint.messageID != req.signedMint.MessageID {
			// The client presented an assignment this bus did not give it. The
			// presented values are NOT echoed — they are attacker-choosable strings
			// headed for a log line — and the MINTED ones are, because those are the
			// bus's own and are what the client should have signed.
			return Result{}, idem.OutcomeNew, fmt.Errorf("%w: agent %q was minted %s (sequence %d) for this %s key", ErrMintMismatch, sender, mint.messageID, mint.seq, op)
		}
		seq = mint.seq
		mintedID = mint.messageID
	}

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
	if err := h.assertSeqFloorLocked(string(op), mintedID, seq); err != nil {
		return Result{}, idem.OutcomeNew, err
	}

	// THE BUS PATH (RELAY-11). A relayed message carries the hops it travelled;
	// everything else is a local send and carries the single hop it has always
	// carried. The default is taken here, at the ONE call site, rather than by
	// letting the constructor treat an empty path as "local": an ingest that
	// silently lost its path would then write a durable record claiming the
	// message originated on this bus, which is a provenance claim nobody made and
	// which nothing downstream could ever tell from a genuine local send.
	busPath := req.busPath
	if len(busPath) == 0 {
		if req.relayed {
			// THE FAILURE THE PARAGRAPH ABOVE NAMES, MADE UNREACHABLE rather than
			// merely warned about. IngestRelayed builds the path and refuses an
			// empty one, so arriving here with none is an internal fault — and the
			// alternative is silently recording a peer's message as having
			// originated on this bus, which nothing downstream could ever detect.
			return Result{}, idem.OutcomeNew, fmt.Errorf("%w: a relayed message reached the durable write path carrying no bus path; defaulting it to this bus would record a provenance claim nobody made", ErrInvalidBusPath)
		}
		busPath = store.LocalBusPath(h.busID)
	}
	m, err := store.NewMessageWithBusPath(h.busID, sender, broadcast, recipients, seq, h.now().UTC(), body, key, req.signedMint.TimestampUnixMilli, req.signedMint.Signature, busPath)
	if err != nil {
		return Result{}, idem.OutcomeNew, err
	}
	// THE RELAY PROVENANCE, STAMPED ONTO THE MESSAGE BEFORE IT IS ENCODED — and
	// the position of these five lines is the whole of RELAY-48.
	//
	// # ANYWHERE AFTER Encode() IS A SILENT NO-OP
	//
	// store.Message.Record() is the only thing that carries these two fields to
	// disk and Encode()'s output IS the wal.Entry body below. But h.store.Append
	// further down populates the store's byOrigin index from the LIVE value, so a
	// writer placed later still makes every in-process lookup succeed. The two
	// fields' ONLY reader is relay.Forwarder.Resume, which runs ONLY after a
	// restart — so the late placement passes every test that does not restart, and
	// destroys a pending onward hop in production. Any test for this MUST restart.
	//
	// # THE HASHES DO NOT MOVE, WHICH IS WHY IT IS SAFE HERE
	//
	// store.Message.SigningMessage omits both fields, and auditContentHash derives
	// from SigningMessage — so the signature this message carries still covers
	// exactly what it covered, and the audit record's content hash is unchanged.
	// The applied-key fingerprint is computed from the request above, not from m.
	//
	// # IT FAILS THE INGEST RATHER THAN WRITING HALF OF IT
	//
	// A relayed message whose attestation is absent or does not bind its sender is
	// refused HERE, before the two-phase write, so nothing durable is created for
	// an obligation this bus could never discharge. store.WithRelayOrigin owns
	// that rule; publish does not restate it.
	if req.relayed {
		m, err = m.WithRelayOrigin(req.originMessageID, req.originAttestation)
		if err != nil {
			return Result{}, idem.OutcomeNew, err
		}
	}
	payload, err := m.Encode()
	if err != nil {
		return Result{}, idem.OutcomeNew, err
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
		return Result{}, idem.OutcomeNew, fmt.Errorf("hub: encoding the applied-key result for message %s: %w", m.ID, err)
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
		return Result{}, idem.OutcomeNew, fmt.Errorf("hub: encoding the applied-key record for message %s: %w", m.ID, err)
	}

	// THE AUDIT RECORD, built and validated BEFORE the durable write, for the
	// same reason the applied-key record above is: a record that cannot be
	// formed must fail the send with NOTHING written, rather than surfacing from
	// inside wal.Begin with a prepare already on disk.
	//
	// It is built HERE, from the message, and not inside the wal.Entry literal
	// below, so that the error names the message it belongs to. See audit.go —
	// in particular, do not "simplify" the content hash to m.ContentSHA256.
	//
	// A RELAYED MESSAGE IS HASHED UNDER THE ORIGIN'S ASSIGNMENT, because its
	// local record — our id, their sender — has no canonical bytes at all and
	// cannot be given any. signedAs carries that substitution and explains it;
	// for a local send it is the zero value and nothing changes.
	//
	// THE SUBSTITUTION IS GATED ON req.relayed, NOT merely on the field being
	// set. auditContentHash honours any non-empty messageID it is given, so
	// deriving the value straight from req.originMessageID would mean a future
	// local caller that set that field — for any reason — silently moved a LOCAL
	// send's audit hash onto an id of its own choosing. Fail closed on the ONE
	// bit that decides it.
	signed := signedAs{}
	if req.relayed {
		signed = signedAs{messageID: req.originMessageID, seq: req.originSeq}
	} else if req.originMessageID != "" || req.originSeq != 0 || len(req.originAttestation.Signature) != 0 {
		// The attestation is checked by its SIGNATURE being present, which is the
		// one field no usable attestation can be missing (attest.Canonicalize is
		// what bounds the rest, and store.WithRelayOrigin is what applies it). It is
		// a BELT on a strap: a local send can only reach here with an attestation if
		// a caller inside this package filled the field and left req.relayed false,
		// in which case the gate above has already declined to stamp it onto the
		// message. Failing loudly beats writing a message that quietly dropped a
		// provenance claim somebody meant to make.
		return Result{}, idem.OutcomeNew, fmt.Errorf("hub: internal: a LOCAL send carried an origin message assignment; the audit content hash of a local send is computed over its own canonical bytes and must never be moved onto another id")
	}
	auditRec, err := auditRecordFor(m, signed)
	if err != nil {
		return Result{}, idem.OutcomeNew, err
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
	// Audit carries the message's audit-log record (DUR-5, invariant 6), built
	// above from this very message. A non-nil Audit that does not validate FAILS
	// the write before anything is appended, which is the fail-closed bargain
	// that makes the trail trustworthy: every field invariant 6 names is
	// present, and the body is not.
	//
	// THE RETURNED wal.Committed IS BOUND, AND ITS CommitIndex BECOMES THE
	// MESSAGE'S DELIVERY POSITION (SIGN-1-FU-REORDER-WATERMARK).
	//
	// This comment used to argue for DISCARDING it, and the point it was making
	// still stands in full: there must be no index-versus-SEQUENCE comparison on
	// this path. SIGN-1's reserve-then-send made the two counters unrelated, the
	// old `committed.PrepareIndex < seq` poison fired on healthy traffic, and
	// reintroducing any such comparison is still forbidden — see the section
	// below.
	//
	// Using the index as a delivery POSITION is a different use and a sound one.
	// It is not compared with a sequence; it is the order in which this bus took
	// responsibility for records, which is exactly what a reader's cursor needs
	// to be expressed in. It is monotone, durable, never reused, and replay
	// reproduces it exactly because replay folds the log in commit order.
	//
	// THE MONOTONICITY RESTS ON writeMu BEING HELD ACROSS BOTH HALVES — this
	// Write and the store.Append below. Do not move either out from under it, and
	// do not add a third caller that appends off this lock: interleaving would
	// let a higher position be applied first, and the lower one would then land
	// below cursors that had already passed it, which is the very suppression
	// this design removed. store.Append cannot detect that for you; it retains
	// the message and shouts.
	committed, err := h.durable.Write(wal.Entry{
		Kind:  store.RecordKind,
		Body:  payload,
		Idem:  encodedIdem,
		Audit: auditRec,
	})
	if err != nil {
		return Result{}, idem.OutcomeNew, fmt.Errorf("hub: durably recording message %s: %w", m.ID, err)
	}
	// THE DELIVERY POSITION, stamped on the in-memory message only. It is NOT
	// part of the durable record — the record's own location in the log IS the
	// position — so nothing above this line changes and store.RecordVersion does
	// not move. Recovery re-stamps it from the same field (Hub.Apply).
	m.Pos = committed.CommitIndex

	// THE RESERVATION IS SPENT, and only now. The message is durable, so the
	// number is unambiguously consumed and no retry may be answered from the
	// mint table again; a retry from here on is answered from the applied-key
	// table, which the durable write above just made recoverable.
	//
	// Everything below this line is already-durable state being reflected into
	// memory, and every failure below POISONS rather than returning, so there is
	// no path on which the mint is deleted for a message that did not land.
	//
	// A RELAYED MESSAGE HAS NOTHING TO SPEND: its number was minted internally
	// above and never existed as a reservation, so mk is the zero mintKey and
	// deleting it would evict whatever a local agent happens to hold under the
	// empty (agent, op, key) tuple. Skipped, not "harmlessly" applied.
	if !req.relayed {
		delete(h.mints, mk)
		h.decMintCountLocked(sender)
	}

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
		return Result{}, idem.OutcomeNew, h.poisoned
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
		return Result{}, idem.OutcomeNew, h.poisoned
	}

	// The DURABLE SENDER-VISIBLE ACCEPTANCE ROW, one per recipient (ACK-2).
	//
	// BEFORE notify, DELIBERATELY, and this ordering is load-bearing rather than
	// tidy. Waking a local waiter hands the message to the recipient, and the
	// recipient's application ACK (ACK-6) is keyed on this very row: wake first
	// and a fast recipient can ACK a message whose lifecycle row does not exist
	// yet, which the ACK route can only answer as "not yours". The row is cheap
	// to have early and impossible to reconstruct late.
	//
	// It NEVER fails the send — see recordAcceptance.
	h.recordAcceptance(m, req.relayed, req.broadcast)

	// LAST, and only here: the message is durable and it is in the serving
	// copy, so a waiter woken now cannot observe something a crash would take
	// back (POLL-2).
	h.notify(m)

	// LAST OF ALL, and only for a federated bus: hand the message to the
	// cross-bus egress path. See forwardOnward for every guarantee this line
	// rests on, and for the ONE hole it deliberately leaves open.
	h.forwardOnward(m)
	return result, idem.OutcomeNew, nil
}

// forwardOnward offers a message this bus has just committed to the OPTIONAL
// cross-bus egress seam. It is a no-op on a bus with no Egress wired.
//
// # WHY IT IS HERE, AT THE VERY END
//
// The message is durable AND it is in the serving copy AND local waiters have
// been woken. Everything the local send promised has already happened, so
// nothing below this line can take any of it back:
//
//   - INVARIANT 4. The local send is acknowledged by its OWN durable write. The
//     forward is a separate best-effort-plus-outbox concern and can never gate,
//     delay or fail that ack. Moving this call ABOVE the durable write would
//     forward a message that might not survive a crash; moving it above notify
//     would let a peer observe a message before a local reader does.
//
//   - A SLOW OR DEAD PEER CANNOT SLOW A LOCAL SEND. That is structural rather
//     than conventional: relay.Forwarder.Enqueue is non-blocking by
//     construction (every queue send is a select with a default arm), so there
//     is no way for this call to end up waiting on a network peer even though
//     writeMu is held across it.
//
//     THAT CLAIM IS ABOUT PEERS AND IS NOT A CLAIM ABOUT DISK, and the two have
//     been confused here before. Enqueue writes a DURABLE OUTBOX RECORD per
//     target before it returns — two fsyncs each, through the same wal.Log this
//     send just committed through — so this call is bounded by the local disk,
//     serially, under the global write lock. For a directed send that is one
//     target. For a BROADCAST it is every configured peer:
//     relay.Registry.BroadcastTargets returns them all regardless of who the
//     recipients are, so at relay.MaxPeers (64) a single broadcast is up to 128
//     serial fsyncs with writeMu held, repeatable by any enrolled local agent.
//
//     That is LATENT rather than live today — /v1/broadcast answers 501, and a
//     relayed broadcast is refused at the far end (relay.ErrUnsignable) — and it
//     is written down here rather than fixed here, because backpressure or
//     batching on this path is a design change, not a tidy-up. Anyone enabling
//     broadcast must deal with it first.
//
//   - A MISBEHAVING IMPLEMENTATION CANNOT TAKE THE SEND DOWN. A panic here would
//     otherwise unwind through publish holding writeMu, killing the process for
//     a message that is already committed and already delivered locally. It is
//     recovered and logged at ERROR instead: the local send stands, the forward
//     is lost, and the loss is loud (invariant 6).
//
// # THE HOLE, NAMED HONESTLY RATHER THAN HIDDEN
//
// The durable outbox record for this forward is written in a SECOND wal
// transaction, inside relay.Forwarder.Enqueue, and NOT in the message's own
// wal.Entry. A crash in the window between the message's commit and the outbox
// enqueue therefore leaves the message durable and delivered locally with the
// forward simply UN-OWED: there is no record of it, so no restart recovers it.
//
// That is a bounded AT-MOST-ONCE window on the CROSS-BUS HOP ONLY. The local
// message is never at risk, and the cross-bus hop is at-least-once everywhere
// else. Folding the outbox record into this message's own wal.Entry would close
// it, and would also change RELAY-15's one-record-one-job shape — so it is
// deliberately NOT done here, and this comment exists so the next reader finds a
// decision rather than an oversight.
func (h *Hub) forwardOnward(m store.Message) {
	if h.egress == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("PANIC in the cross-bus egress seam; this message is durable and was delivered to local readers, but it will NOT be forwarded to any peer and no restart brings that forward back",
				"message_id", m.ID,
				"seq", m.Seq,
				"sender", m.Sender,
				"panic", fmt.Sprint(r),
			)
		}
	}()
	h.egress.Forward(m)
}

// recordAcceptance writes ONE durable sender-visible lifecycle row per
// recipient for a message this bus has just committed (ACK-2; ACK-CONTRACT.md
// §7, §8.2 event E1). It is a no-op on a bus with no AckRecorder wired.
//
// # WHAT IT RECORDS, AND WHAT THAT SENTENCE MAY NEVER BE STRETCHED INTO
//
// `accepted` means "this bus has committed and fsynced the message" and NOTHING
// MORE. It is not "delivered", it is not "the recipient has it", and it is not
// "a peer took it". The one sentence this whole epic exists to stop anyone
// writing is "the send returned success, so the message was delivered".
//
// # IT NEVER FAILS THE SEND. THAT IS A DELIBERATE ASYMMETRY, NOT AN OVERSIGHT
//
// Everywhere else in this repository the fail-closed answer is to refuse the
// operation. Here the message is ALREADY on stable storage and the sender is
// ALREADY owed its 201 (invariant 4), so refusing would mean an OBSERVABILITY
// table causing a MESSAGING outage — it would break everything while violating
// nothing. Degrading the observation is recoverable; refusing the send is not.
// The recorder's own capacity refusals are logged loudly and specifically at the
// point of refusal (invariant 6), and anything unexpected is logged here.
//
// # WHICH MESSAGES GET A ROW, AND WHY BROADCAST DOES NOT
//
//   - BROADCAST: none, and this is a DELIBERATE NON-GOAL, not an omission. A
//     broadcast has NO canonical audience under signing format v1 —
//     store.Message keeps it as a FLAG, deliberately not an expanded roster — so
//     there is no (message, recipient) pair to key a row on. Inventing one would
//     settle SIGN-3 by accident, in a place nobody would look. /v1/broadcast
//     answers 501 today, so this arm is unreachable from the agent surface; it is
//     written as a guard rather than an assumption. The same reasoning is stated
//     at transitAck's empty-recipient-list arm and in
//     internal/store/provenance.go; do NOT drop the broadcast early-return
//     silently.
//   - RELAYED INGEST: ONE ROW PER RECIPIENT, as of ACK-12-FU-DESTINATION-ROW.
//     The row is keyed on the ORIGIN message id (m.OriginID() ==
//     m.OriginMessageID for a relayed copy, invariant 1 — the origin bus's id,
//     never re-minted or adopted here) and the recipient, and charged to
//     m.Sender (the origin agent's fully-qualified id, invariant 2). The original
//     sender on the ORIGIN bus still cannot read THIS bus's row (§13.3), so the
//     row buys no sender-visible observation on an intermediate or terminal bus;
//     what it buys is transit-ack AUTHORISATION that outlives message-body
//     pruning — hub.transitAck reads it, bounding the ack window by ack.Retention
//     (24h) rather than by the byte/age message retention that prunes the body
//     first. This became correct once ACK-4-FU-RECIPIENT-BINDING landed (several
//     rows behind one correlation key is now bound to the authenticated peer).
//
// # THE CORRELATION KEY IS READ THROUGH OriginID(), NEVER RE-SPELLED
//
// store.Message.OriginID() is the ONE place the "origin id when set, local id
// otherwise" rule is written down, and its doc comment forbids re-spelling that
// branch at a call site. For a locally-originated message it returns the
// message's own id, which IS the origin id.
//
// # THE COST, MEASURED RATHER THAN ESTIMATED — UP TO 64 FSYNCS PER RELAYED INGEST
//
// The loop below is over m.Recipients, and each row is a separate two-phase
// transaction through the same log, under writeMu. A LOCAL send still runs it
// EXACTLY ONCE: SendRequest carries a single `To` (publish is called with
// `[]string{req.To}`) and a broadcast is excluded above.
//
// THE RELAYED MULTI-RECIPIENT CASE IS NOW LIVE (ACK-12-FU-DESTINATION-ROW).
// A relayed ingest carries 1..store.MaxRecipients (64) recipients, so this loop
// can now run up to 64 times = up to 64 serial two-phase fsyncs under the global
// writeMu, and it is driven by an authenticated PEER, not by the local sender.
// That is exactly the latent hazard the earlier version of this note told the
// next task to re-check "before any task gives a local send several recipients"
// — except it arrived on the relay ingest path instead. The fix is to BATCH the
// rows into ONE wal.Entry, which is a record-shape change and is OUT OF SCOPE
// here; this task deliberately does not implement it.
//
// # THE CRASH WINDOW, NAMED RATHER THAN HIDDEN
//
// The row is written in a SECOND wal transaction, after the message's own
// commit. A crash in between leaves the message durable with no lifecycle row,
// so the sender is later told `unknown` rather than `accepted`. That is a
// bounded loss of OBSERVATION and never of the message, it is exactly what
// §11.3's capacity refusal already produces by design, and reconstructing state
// across a crash boundary is ACK-8's (§14 D1).
//
// IT COULD BE CLOSED, AND WAS NOT. An earlier draft of this comment said folding
// the row into the message's own wal.Entry was "not possible, a wal.Entry
// carries exactly one Kind". That is FALSE and it is worth correcting in place
// rather than deleting, because it is the kind of false impossibility that stops
// the next reader looking: `auth.EnrolInviteRecordKind = "agent+invite"` is a
// COMPOSITE kind whose entire purpose is one entry, one transaction, two effects
// — it exists precisely to close a window of this class
// (CONTRACTS-ONDISK.md, "A composite Entry.Kind").
//
// So the reason is a TRADE, not a limitation. A composite "message+ack" kind
// would change the discriminator on EVERY message record this bus writes, split
// the message applier, and oblige every existing log to be read under both
// spellings — a migration across the whole message plane, taken to close a
// window that costs an observation and never a message, for a table nothing
// reads yet. If ACK-8 judges the window unacceptable, that is the shape to
// reach for.
func (h *Hub) recordAcceptance(m store.Message, relayed, broadcast bool) {
	// BROADCAST STAYS A DELIBERATE NON-GOAL — see "WHICH MESSAGES GET A ROW"
	// above. It has no canonical audience under signing format v1, so there is no
	// (message, recipient) pair to key a row on; do NOT drop this early-return
	// silently. RELAYED ingest, by contrast, NOW writes rows (removing `relayed`
	// from this guard is ACK-12-FU-DESTINATION-ROW): m.OriginID() ==
	// m.OriginMessageID for a relayed copy and m.Sender is the origin agent's
	// fully-qualified id, so the existing loop writes one row per recipient keyed
	// (OriginMessageID, recipient), charged to the origin sender.
	if h.acks == nil || broadcast {
		return
	}
	correlationKey := m.OriginID()
	for _, recipient := range m.Recipients {
		// THE RECOVER IS PER RECIPIENT, NOT AROUND THE LOOP. A panic recovered
		// outside the loop would abandon rows 2..N as well as the one that
		// panicked, turning one bad recipient into a silent loss of status for
		// every other recipient of the same message. This is now LIVE on the
		// relayed-ingest path (up to 64 recipients, see the cost note above), so
		// the per-recipient scope is load-bearing rather than latent.
		err := h.acceptOne(m, correlationKey, recipient)
		if err == nil {
			continue
		}
		// A CAPACITY REFUSAL IS NOT LOGGED HERE, AND THAT IS NOT A SOFTENING.
		//
		// The recorder logs those itself, at ERROR, with the full remedy and a
		// running total — and THROTTLED, to one line per minute, precisely
		// because a full table refuses on every send and an unthrottled line
		// would emit thousands per second. Logging them again here, unthrottled,
		// on the send path, would defeat that throttle completely: an
		// OBSERVABILITY table would become a DISK outage, which is the exact
		// failure §11.3 exists to prevent. It would also be the second copy of a
		// line the recorder already emits.
		//
		// What is left is everything the recorder does NOT already account for —
		// a nil log, a validation refusal, an fsync failure — and those are rare
		// by construction and must never be quiet.
		if errors.Is(err, ack.ErrCapacity) || errors.Is(err, ack.ErrAgentQuota) {
			continue
		}
		h.log.Error("NO SENDER-VISIBLE DELIVERY STATUS was recorded for this recipient, so GET /v1/ack will report `unknown` for it. THE MESSAGE IS DURABLE AND WAS ACCEPTED — this degrades the observation and never the send",
			"message_id", m.ID,
			"correlation_key", correlationKey,
			"sender", m.Sender,
			"recipient", recipient,
			"err", err,
		)
	}
}

// acceptOne records ONE recipient's acceptance row and converts a panic in the
// recorder into an error.
//
// A panic here would otherwise unwind through publish holding writeMu and kill
// the process for a message that is already committed and already in the serving
// copy — the same containment forwardOnward applies to the egress seam, and for
// the same reason. It is a separate function only so the recover() is scoped to
// ONE recipient; see the call site.
func (h *Hub) acceptOne(m store.Message, correlationKey, recipient string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PANIC in the delivery lifecycle seam: %v", r)
		}
	}()
	return h.acks.Accept(correlationKey, m.Sender, recipient)
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
