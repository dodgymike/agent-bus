package relay

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// DefaultQueueDepth is the per-peer outbound queue depth used when
// ForwarderOptions.QueueDepth is zero.
const DefaultQueueDepth = 256

// DefaultForwardTimeout bounds ONE outbound relay attempt.
//
// Without it a peer that accepts a connection and then never answers pins the
// goroutine serving its queue forever, which would quietly convert a
// per-peer-queue design back into an unbounded resource leak. Thirty seconds is
// far above any healthy round trip and far below "forever".
const DefaultForwardTimeout = 30 * time.Second

// The RETRY HORIZON, and the one constraint RELAY-4 is not free to choose.
//
// # It is bounded by internal/idem, and the bound is a PROMISE, not a preference
//
// internal/idem/retention.go derives the applied-key retention window as
//
//	2 x (PeerOutageBudget + SessionLifetimeMax + ParkedPollMax + TransportRetryHorizon)
//
// and its comment on PeerOutageBudget names THIS task explicitly: "RELAY-4 is
// NOT yet implemented, so this is not read off an existing ceiling — it is the
// BUDGET RELAY-4 must design within. Stated as a constraint, not an observation:
// if RELAY-4's total retry horizon ever exceeds this, a returning peer's retry
// falls outside the window and is applied as a new operation."
//
// That is the single by-design double-apply in the system, and exceeding the
// budget would convert it from "a retry later than 50 hours" into "a retry from
// this forwarder, on an ordinary day". So the ceiling is enforced STRUCTURALLY
// in NewForwarder (a forwarder that could out-retry the window will not
// construct) and asserted in TestPeerRetryBackoffHorizonStaysInsideTheOutageBudget,
// rather than left as a comment nobody re-checks.
//
// # What "total retry horizon" means here, precisely
//
// It is measured from the moment the job was ENQUEUED, not from its first
// attempt — and that distinction is the whole reason the bound holds. Per-peer
// queues are drained SERIALLY, so with a first-attempt anchor a job sitting
// behind a dead peer's head-of-line job would start its own full horizon after
// that job's horizon had already elapsed, and two 23-hour horizons in a row is
// 46 hours of retrying against a 24-hour budget. Anchoring on the enqueue
// instant makes every job's last attempt fall within one horizon of the local
// send that produced it, whatever happened ahead of it in the queue; a job whose
// horizon elapsed while it waited is dropped at dequeue and counted in
// Dropped.Expired.
//
// The last attempt may START at the deadline and then run for a full Timeout, so
// the quantity the ceiling actually constrains is RetryHorizon + Timeout.
const (
	// RetryHorizonCeiling is the hard upper bound on RetryHorizon + Timeout:
	// idem.PeerOutageBudget, cited by reference so the two cannot drift apart.
	RetryHorizonCeiling = idem.PeerOutageBudget

	// DefaultRetryHorizon is the ceiling minus the worst-case overrun of the
	// final attempt — 23h59m30s. Derived, not chosen: it is the largest horizon
	// that still guarantees the last outbound byte leaves inside the budget.
	DefaultRetryHorizon = RetryHorizonCeiling - DefaultForwardTimeout

	// DefaultRetryBackoffBase is the first backoff WINDOW — the first retry is
	// drawn from [0, 1s), not slept for a full second. One second as the window
	// puts the first retry's EXPECTED wait (500ms) above a healthy round trip
	// while keeping the worst case far below the interval at which a human would
	// call the link "down".
	DefaultRetryBackoffBase = time.Second

	// MinRetryBackoffBase is the floor on a configured base. Ten milliseconds
	// is where a retry loop stops being a backoff and starts being a load
	// generator aimed at a peer that is already failing: with base = cap = 1ms a
	// single peer takes roughly a thousand requests a second, sustained for the
	// whole horizon.
	MinRetryBackoffBase = 10 * time.Millisecond

	// MaxRetryBackoffCap is the ceiling on a configured cap: the retry horizon
	// itself, since a backoff longer than the horizon cannot produce even a
	// second attempt. It also keeps the doubling in backoffWindow far away from
	// int64-nanosecond overflow, which would silently invert the schedule.
	MaxRetryBackoffCap = RetryHorizonCeiling

	// DefaultRetryBackoffCap is the longest this forwarder will leave a peer
	// unprobed, and it is idem.ParkedPollMax (5 minutes) by reference rather
	// than by coincidence. That is already the longest quiet period this bus
	// treats as normal — the hard ceiling on a parked long poll — so a peer that
	// comes back is noticed within an interval the system elsewhere considers
	// unremarkable. Larger would be cheaper and slower to recover; smaller would
	// hammer a bus that is deliberately shedding load.
	DefaultRetryBackoffCap = idem.ParkedPollMax
)

// ErrForwarderClosed reports Enqueue on a Forwarder that has been Closed.
//
// It is the ONLY error Enqueue returns, and it describes a LOCAL LIFECYCLE
// fault (shutdown, or a bug on this bus), never a peer condition. A caller must
// never turn it into a failure of the local send that triggered the forward:
// relay is a background concern, and a send's success is settled by the durable
// local write, not by what any peer did with it afterwards.
var ErrForwarderClosed = errors.New("relay: forwarder is closed")

// DropCounts breaks down why messages were not forwarded.
//
// "DROPPED" MEANS "NOT SENT NOW", AND WITH AN OUTBOX CONFIGURED THAT IS NO
// LONGER THE SAME AS "LOST". Read each counter against whether RELAY-19 wrote a
// terminal outbox record for it:
//
//   - Expired, Permanent, Yielded and attempt's no-route arm SETTLE the job
//     `abandoned`. The message is genuinely lost, and the durable record says
//     so by name (invariant 6).
//   - Full does NOT settle anything. The pending record stands, so the job is
//     still owed and Resume re-offers it after the next restart. Nor does
//     RESUME's OWN no-route arm — the one place NoRoute means "later" rather
//     than "never", for the reason resumeJob sets out.
//   - NotDurable never had a record to begin with.
//
// With no outbox configured every one of them is a silent loss, which is what
// the Forwarder doc means by BEST EFFORT.
type DropCounts struct {
	// Full: the peer's queue was at its depth, so nothing was offered onto it.
	//
	// WITHOUT an outbox this is a LOSS BY DESIGN — see Forwarder. WITH one the
	// job stays PENDING and is re-offered by Resume on the next start: an
	// in-memory bound is a reason to send later, never a reason to destroy work
	// this bus has already durably accepted responsibility for.
	Full uint64
	// Loop: the split horizon refused the target, or the message's own path
	// already contained us.
	Loop uint64
	// NoRoute: no peer owns the recipient, or the peer has no known base URL.
	//
	// IT IS THE ONE COUNTER THAT MEANS TWO DIFFERENT THINGS, so it is spelled
	// out rather than left to be discovered. Raised at Enqueue or by attempt it
	// is FINAL — the peer was de-peered or moved, and the job is abandoned.
	// Raised by RESUME it is not: the job stays owed, because at startup the
	// identical reading is what an unloaded peer roster looks like. See
	// resumeJob.
	NoRoute uint64

	// Expired: the job's retry horizon ran out — either it sat in the queue
	// behind a dead peer until its deadline passed, or it was retried until the
	// deadline. THIS IS THE PEER-IS-DOWN NUMBER an operator watches: it rising
	// means a peer has been unreachable for longer than the outage budget the
	// applied-key retention window is derived from.
	Expired uint64

	// Yielded: a retriable failure was abandoned because this peer's queue was
	// FULL and the retrying job was holding its head. One message is dropped so
	// the rest of the peer's queue can move.
	//
	// READ IT AGAINST Full, NOT AGAINST Expired. Two different conditions raise
	// it, and an earlier version of this comment named only one:
	//
	//   - Yielded rising, Full LOW — a POISON MESSAGE on a healthy peer. One
	//     envelope keeps failing while the peer answers everything else.
	//   - Yielded rising, Full HIGH — a SATURATED QUEUE, which is what a peer
	//     that is genuinely down plus steady traffic looks like. Every job then
	//     gets ONE attempt and is yielded before its own deadline, so Expired
	//     stays near zero even though nothing is getting through.
	//
	// The second case is worth stating plainly: AT SATURATION, RETRY DEGRADES TO
	// BEST-EFFORT-ONCE. That is the intended trade — the alternative is holding
	// the head while everything behind it is dropped — but it means a high
	// Yielded is not evidence that retry is working.
	Yielded uint64

	// Permanent: the peer refused with a status that resending cannot change —
	// a 4xx or a 3xx (see PeerRefusedError.Retriable), or an envelope this bus
	// could not even encode. Counted apart from Expired because they demand
	// opposite responses: Expired means fix the link, Permanent means fix the
	// message or the peering.
	Permanent uint64

	// NotDurable: the delivery could not be RECORDED, so it was never queued.
	// Either the outbox refused it (ErrOutboxCapacity — this peer's or the
	// bus's pending-work quota is full) or the durable write itself failed.
	//
	// IT IS COUNTED APART FROM Full BECAUSE THE REMEDY IS DIFFERENT AND SO IS
	// THE OUTCOME. A Full drop is a message we still owe and will re-offer; a
	// NotDurable drop is a message that was never taken responsibility for at
	// all, and no restart brings it back. Rising NotDurable means the outbox
	// backlog is not draining, or the disk is not accepting writes.
	NotDurable uint64
}

// ForwarderStats is the observable state of a Forwarder. Queued counts jobs
// accepted onto a queue; Sent counts peer answers received (including a loop
// drop, which is a settled answer); Failed counts transport or refusal errors.
type ForwarderStats struct {
	Queued  uint64
	Sent    uint64
	Dropped DropCounts
	Failed  uint64

	// Retried counts ATTEMPTS that were not the first attempt for their job.
	// Failed counts failed attempts, so Failed-minus-Retried separates "one
	// peer is flapping a lot" from "many messages failed once", which the two
	// numbers cannot be told apart from either alone.
	Retried uint64

	// Workers is how many per-peer goroutines are currently running — the
	// "one goroutine per peer" design made visible. It is what an operator
	// reads to confirm the forwarder is not accumulating workers, and it is
	// what makes "no goroutine outlives Close" a checkable claim rather than
	// an assertion about the code: after Close returns it is always 0.
	Workers int64
}

// ForwarderOptions configures NewForwarder.
type ForwarderOptions struct {
	// BusID is THIS bus's id — the hop appended to every outbound path.
	BusID string

	// Registry resolves recipients to peers and supplies the broadcast fan-out.
	// Required.
	Registry *Registry

	// Client is the initiator used for every outbound POST. Required, and it
	// carries the link's TLS material (see ClientConfig.HTTPClient).
	Client *Client

	// PeerBaseURL reports where a peer lives. Required: without it the
	// forwarder would have a routing decision and nowhere to send it, and
	// defaulting it to anything at all would mean guessing at an address.
	//
	// It is called once per Enqueue (targets resolution) and, separately, once
	// per attempt by every per-peer worker goroutine — see Forwarder.attempt's
	// "THE ADDRESS IS RE-RESOLVED ON EVERY ATTEMPT" — so it MUST BE SAFE FOR
	// CONCURRENT USE, the same requirement ClientConfig.LocalRoster and
	// handshake.Config.LocalRoster already state on themselves. Registry's own
	// PeerBaseURL method satisfies this (it takes Registry's RLock) and is the
	// intended value here rather than a hand-written closure over a Registry.
	PeerBaseURL func(peerBusID string) (string, bool)

	// QueueDepth is the per-peer queue depth. 0 means DefaultQueueDepth;
	// negative is a construction error.
	QueueDepth int

	// Timeout bounds one outbound attempt. 0 means DefaultForwardTimeout;
	// negative is a construction error.
	Timeout time.Duration

	// RetryHorizon bounds how long ONE job may keep being retried, measured
	// from the instant it was enqueued. 0 means DefaultRetryHorizon; negative
	// is a construction error.
	//
	// RetryHorizon + Timeout MUST NOT EXCEED RetryHorizonCeiling
	// (idem.PeerOutageBudget). NewForwarder refuses options that do, because a
	// forwarder that can out-retry the applied-key retention window turns the
	// one by-design double-apply in the system into an everyday one. See the
	// derivation above DefaultRetryHorizon.
	RetryHorizon time.Duration

	// RetryBackoffBase is the first backoff window; each subsequent window
	// doubles until it reaches RetryBackoffCap. 0 means
	// DefaultRetryBackoffBase; negative is a construction error.
	RetryBackoffBase time.Duration

	// RetryBackoffCap is the largest backoff window. 0 means
	// DefaultRetryBackoffCap; negative, or smaller than the base, is a
	// construction error.
	RetryBackoffCap time.Duration

	// Outbox makes cross-bus delivery DURABLE (RELAY-19). It is OPTIONAL, and
	// what it costs when it is absent is spelled out in the Forwarder doc:
	// without it this forwarder is exactly the best-effort, in-memory one
	// RELAY-4 left behind.
	//
	// The message handed to Enqueue must carry Size and ContentSHA256 —
	// len(Body) and its hex SHA-256 — because those are the two facts the record
	// keeps so that a job REBUILT at recovery can be CHECKED against the message
	// it was created for rather than assumed to match it. A RelayedMessage that
	// came out of ValidateRelayRequest always has them.
	Outbox *Outbox

	// RecoverMessage reads a message back by its ORIGIN message id so Resume can
	// rebuild the envelope for a pending job. REQUIRED whenever Outbox is set,
	// and refused when it is not: a durable outbox nothing can replay from is a
	// table of jobs that will never move again, which is worse than no outbox at
	// all because it also consumes the pending quota.
	//
	// The outbox record deliberately does NOT carry the body (invariant 6, and
	// see outbox.go's file comment): the body is already durable exactly once in
	// this bus's own message record, and a second copy per peer could disagree
	// with the first. So the wiring site supplies the lookup — store.Message
	// retains every field the envelope needs.
	//
	// ok=false means "no such message" and is a settled, ABANDONED job: a job
	// naming a message this bus can no longer produce is not deliverable and
	// saying so durably is invariant 6's requirement. A non-nil error is treated
	// identically — Resume cannot tell a missing message from an unreadable one,
	// and guessing in the other direction would mean retrying a job forever.
	//
	// It is called once per pending job during Resume, on Resume's goroutine.
	RecoverMessage func(originMessageID string) (RelayedMessage, bool, error)

	// Logger is optional; nil discards.
	Logger *logging.Logger
}

// relayJob is one outbound attempt: an already-built envelope, where it goes,
// and WHEN IT WAS ENQUEUED.
//
// enqueuedAt is not diagnostic decoration — it is the anchor of the retry
// deadline, and anchoring on it rather than on the first attempt is what keeps
// the total retry horizon inside idem.PeerOutageBudget when jobs queue up behind
// a dead peer. See the RetryHorizonCeiling derivation.
type relayJob struct {
	peerBusID  string
	baseURL    string
	req        RelayRequest
	enqueuedAt time.Time

	// jobID names this job's DURABLE outbox record, and is empty when no outbox
	// is configured. Every settlement in this file is keyed on it, so a job
	// carrying "" is silently non-durable — which is the correct behaviour for a
	// forwarder built without an Outbox and the reason settle() checks it rather
	// than assuming.
	jobID string

	// lastErr is the most recent attempt failure, carried so the DURABLE
	// abandonment can say what actually went wrong instead of "permanently
	// refused". It is untrusted text (a peer's status line can reach it) and is
	// bounded by sanitiseOutboxReason on the way into the record.
	lastErr string
}

// Forwarder relays messages to peers IN THE BACKGROUND.
//
// # A SLOW OR DEAD PEER MUST NEVER MAKE A LOCAL SEND SLOW OR FAIL
//
// That is RELAY-4's rule, applied now and enforced STRUCTURALLY rather than by
// convention: Enqueue is non-blocking by construction, so there is no way for a
// caller on a request path to end up waiting on a peer even if it wanted to. A
// local send's success is settled by its own durable write (invariant 4); what
// a peer does with a copy afterwards is a separate, best-effort concern, and
// coupling the two would let any federated bus degrade every local send.
//
// # ONE BOUNDED QUEUE AND ONE GOROUTINE PER PEER
//
// Not a shared queue with N workers. With a shared pool, ONE dead peer with a
// long dial timeout occupies workers one message at a time until every worker
// is blocked on it, and healthy peers stop being served entirely —
// head-of-line blocking, where the failure of the least important peer takes
// out the most important one. Per-peer queues confine a dead peer's damage to
// its own queue: it fills, its own messages are dropped and counted, and every
// other peer's queue is untouched.
//
// # RETRY IS PER-PEER AND SERIAL, AND THAT COSTS SOMETHING. SAY SO.
//
// RELAY-4 adds retry inside the peer's own goroutine, so a failing job is
// retried with exponential backoff and full jitter until it succeeds, is
// permanently refused, or its horizon runs out. Because the goroutine is the
// peer's ONLY worker, a job being retried HOLDS THE HEAD OF THAT PEER'S QUEUE.
// That is deliberate — it preserves per-peer ordering and keeps a dead peer's
// cost inside its own queue — but the consequence must not be glossed: while a
// peer is down, its queue fills behind the retrying head and the messages behind
// it are dropped (Dropped.Full), and any that survive long enough to pass their
// own deadline are dropped as Dropped.Expired at dequeue.
//
// The net effect is a large win for a FLAPPING or restarting peer, which is
// RELAY-4's actual case, and no change at all for a peer that is simply gone:
// both before and after this change, everything queued for a permanently dead
// peer is eventually lost. Only a durable outbox fixes the second case, and it
// is a separate follow-up.
//
// # THE QUEUE IS STILL IN-MEMORY. WHAT CHANGED IS THAT IT IS NO LONGER THE ONLY
// # RECORD (RELAY-19)
//
// The paragraph that stood here said cross-bus delivery is BEST EFFORT and that
// nothing in the product should claim otherwise. It was TRUE until this task,
// and it is retracted NOW rather than earlier because retracting it before the
// wiring existed would have been the false claim this repo keeps deleting.
//
// The retraction is CONDITIONAL, and the condition is a field, not a promise:
//
//   - WITH ForwarderOptions.Outbox SET, a delivery is written to the durable
//     outbox as `pending` and FSYNCED BEFORE it is offered to any queue, and it
//     is settled `delivered`/`abandoned` only once its outcome is known. A CRASH
//     with the queue non-empty therefore loses nothing: the pending records
//     survive, and Resume re-offers them after the next start. So does an
//     ordinary SHUTDOWN — the four paths that used to drop a job silently now
//     leave it owed.
//   - WITH NO OUTBOX the old paragraph still applies WORD FOR WORD: the queue is
//     process memory, a crash loses it, a full queue drops it, and cross-bus
//     delivery is BEST EFFORT. Nothing in the product may claim otherwise for a
//     forwarder built this way, and NewForwarder logs the fact at construction
//     so a mis-wired deployment is visible in the log rather than only in a
//     lost message.
//
// # WHAT IS STILL LOST WITH AN OUTBOX — THE COMPLETE LIST, BECAUSE A SHORT ONE
// # IS THE MORE DANGEROUS COMMENT
//
// An earlier draft of this paragraph said "TWO THINGS", named the horizon and a
// permanent refusal, and stopped. outbox.go's upsertLocked names that exact
// trap: an enumeration declared CLOSED while omitting a member tells the next
// reader to stop looking. Every arm below settles the job ABANDONED — counted,
// and logged by name, which is what invariant 6 asks — or, in the last case,
// never records it at all:
//
//	Dropped.Expired    the RETRY HORIZON ran out, in the queue or across
//	                   retries. Kept as a LOSS deliberately: retrying past
//	                   idem.PeerOutageBudget is a DUPLICATE delivery, which is
//	                   worse.
//	Dropped.Permanent  the peer refused with a status resending cannot change.
//	Dropped.Yielded    the head of a FULL queue was yielded after a retriable
//	                   failure, so one message is lost instead of every message
//	                   behind it. NOTE THIS QUALIFIES THE CLAIM ABOVE: a full
//	                   queue does not destroy the message being OFFERED, but it
//	                   is exactly what destroys the message at the HEAD.
//	Dropped.NoRoute    the peer was de-peered, or its address moved, after the
//	                   message was queued. Abandoned by design — see attempt.
//	                   RESUME's no-route arm is NOT in this list: it leaves the
//	                   job owed, because at startup the same reading is what an
//	                   unloaded peer roster looks like.
//	(Resume)           a recovered job whose message cannot be read back,
//	                   whose content no longer matches the record, or whose
//	                   envelope cannot be rebuilt.
//	Dropped.NotDurable the outbox refused the enqueue or the durable write
//	                   failed, so the delivery was never recorded and never
//	                   offered. The only arm with no durable trail — there was
//	                   nothing to write it to.
//
// Dropped.Full and the four SHUTDOWN paths are NOT in this list, and that is the
// change RELAY-19 makes: they leave the job PENDING, still owed, and Resume
// re-offers it.
type Forwarder struct {
	busID        string
	reg          *Registry
	client       *Client
	peerBaseURL  func(string) (string, bool)
	depth        int
	timeout      time.Duration
	retryHorizon time.Duration
	backoffBase  time.Duration
	backoffCap   time.Duration
	log          *logging.Logger

	// outbox is the durable delivery table, or nil for the best-effort
	// forwarder. recoverMessage rebuilds an envelope for Resume.
	outbox         *Outbox
	recoverMessage func(string) (RelayedMessage, bool, error)

	// rand is THIS forwarder's own jitter source, seeded from crypto/rand. It
	// is not the global math/rand: under go.mod's go 1.19 that source is seeded
	// with 1, so every process draws the same sequence and full jitter stops
	// decorrelating anything across buses. See jitter.
	//
	// IT IS FOR TIMING ONLY AND MUST NEVER PRODUCE SECURITY MATERIAL. math/rand
	// is not a CSPRNG whatever it was seeded from, and this stream is observable
	// in the retry timing a peer can watch. That restriction is precisely what
	// makes cryptoSeed's wall-clock fallback acceptable — anything needing
	// unpredictability reads crypto/rand directly (invariant 9).
	randMu sync.Mutex
	rand   *rand.Rand

	// jitterFn is the one test seam: it exists so the backoff SCHEDULE can be
	// asserted exactly rather than inferred from wall-clock timings, which is
	// the classic flaky test. Nil means the real draw.
	jitterFn func(time.Duration) time.Duration

	// stopping is closed by Close so a worker parked in a backoff abandons it
	// immediately. Without it a graceful Close would block for up to
	// backoffCap on every peer that happened to be in a retry.
	stopping chan struct{}

	// ctx/cancel abort in-flight requests when Close gives up waiting, so no
	// goroutine can outlive Close even when a peer is hanging.
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	// resumed makes Resume ONCE-ONLY, which is the mechanism behind
	// "re-enqueued exactly once": a second Resume would offer a second copy of
	// every job still pending from the first. warnedUnresumed keeps the
	// forward-before-Resume warning to one line per process.
	resumed         bool
	warnedUnresumed bool
	queues          map[string]chan relayJob
	wg              sync.WaitGroup

	queued         atomic.Uint64
	sent           atomic.Uint64
	failed         atomic.Uint64
	retried        atomic.Uint64
	dropFull       atomic.Uint64
	dropLoop       atomic.Uint64
	dropNoRoute    atomic.Uint64
	dropExpired    atomic.Uint64
	dropPermanent  atomic.Uint64
	dropYielded    atomic.Uint64
	dropNotDurable atomic.Uint64
	workers        atomic.Int64
}

// NewForwarder validates opts and returns a Forwarder with no goroutines
// running yet — one is started per peer on that peer's first enqueue, so a bus
// federated with 64 peers that only ever talks to two does not hold 62 idle
// goroutines.
func NewForwarder(opts ForwarderOptions) (*Forwarder, error) {
	if err := ids.ValidateBusID(opts.BusID); err != nil {
		return nil, fmt.Errorf("relay: forwarder bus id: %w", err)
	}
	if opts.Registry == nil {
		return nil, errors.New("relay: ForwarderOptions.Registry is required; without it there is nothing to resolve a recipient to a peer")
	}
	if opts.Client == nil {
		return nil, errors.New("relay: ForwarderOptions.Client is required; it carries the mutual-TLS configuration of every bus-to-bus link")
	}
	if opts.PeerBaseURL == nil {
		return nil, errors.New("relay: ForwarderOptions.PeerBaseURL is required; a routing decision with no address to send it to would have to be guessed at")
	}
	// A DURABLE OUTBOX WITHOUT A WAY TO REPLAY FROM IT IS REFUSED AT
	// CONSTRUCTION, not discovered at the restart that needed it. Such a
	// forwarder would write pending records nothing could ever turn back into an
	// envelope: every recovered job would sit owed until its horizon ran out,
	// holding its peer's pending quota the whole time, and the bus would look
	// durable while delivering strictly less than the best-effort one.
	if opts.Outbox != nil && opts.RecoverMessage == nil {
		return nil, errors.New("relay: ForwarderOptions.RecoverMessage is required whenever Outbox is set; a pending outbox record carries routing facts and NOT the body, so Resume cannot rebuild the envelope without a way to read the message back by its origin id")
	}
	// The mirror image: a lookup with no outbox is a wiring mistake in the other
	// direction, and a silent one — the forwarder would run best-effort while the
	// caller believed it had wired durability.
	if opts.Outbox == nil && opts.RecoverMessage != nil {
		return nil, errors.New("relay: ForwarderOptions.RecoverMessage is set but Outbox is nil; nothing would ever call it, and a forwarder with no outbox is BEST EFFORT (see the Forwarder doc) rather than durable")
	}
	if opts.QueueDepth < 0 {
		return nil, fmt.Errorf("relay: ForwarderOptions.QueueDepth is %d; it must be zero (meaning %d) or positive", opts.QueueDepth, DefaultQueueDepth)
	}
	if opts.Timeout < 0 {
		return nil, fmt.Errorf("relay: ForwarderOptions.Timeout is %s; it must be zero (meaning %s) or positive", opts.Timeout, DefaultForwardTimeout)
	}
	if opts.RetryHorizon < 0 {
		return nil, fmt.Errorf("relay: ForwarderOptions.RetryHorizon is %s; it must be zero (meaning %s) or positive", opts.RetryHorizon, DefaultRetryHorizon)
	}
	if opts.RetryBackoffBase < 0 {
		return nil, fmt.Errorf("relay: ForwarderOptions.RetryBackoffBase is %s; it must be zero (meaning %s) or positive", opts.RetryBackoffBase, DefaultRetryBackoffBase)
	}
	if opts.RetryBackoffCap < 0 {
		return nil, fmt.Errorf("relay: ForwarderOptions.RetryBackoffCap is %s; it must be zero (meaning %s) or positive", opts.RetryBackoffCap, DefaultRetryBackoffCap)
	}
	depth := opts.QueueDepth
	if depth == 0 {
		depth = DefaultQueueDepth
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultForwardTimeout
	}
	horizon := opts.RetryHorizon
	if horizon == 0 {
		horizon = DefaultRetryHorizon
	}
	backoffBase := opts.RetryBackoffBase
	if backoffBase == 0 {
		backoffBase = DefaultRetryBackoffBase
	}
	backoffCap := opts.RetryBackoffCap
	if backoffCap == 0 {
		backoffCap = DefaultRetryBackoffCap
	}
	if backoffCap < backoffBase {
		return nil, fmt.Errorf("relay: ForwarderOptions.RetryBackoffCap (%s) is below RetryBackoffBase (%s); the schedule would go backwards on its first doubling", backoffCap, backoffBase)
	}
	// A FLOOR AND A CEILING ON THE SCHEDULE, for the same reason the horizon has
	// one: both ends of the range are a way to turn a configuration knob into a
	// defect. Below the floor the forwarder becomes a load generator aimed at a
	// peer that is already failing (base = cap = 1ms is ~1000 requests/second at
	// one peer, sustained for the whole horizon). Above the ceiling the doubling
	// in backoffWindow overflows int64 nanoseconds to a NEGATIVE duration, jitter
	// returns 0, and the retry loop spins with no sleep at all — the opposite of
	// a backoff, produced by asking for too much of one.
	if backoffBase < MinRetryBackoffBase {
		return nil, fmt.Errorf("relay: ForwarderOptions.RetryBackoffBase is %s, below the %s floor; a shorter one turns retry into a load generator aimed at a peer that is already failing", backoffBase, MinRetryBackoffBase)
	}
	if backoffCap > MaxRetryBackoffCap {
		return nil, fmt.Errorf("relay: ForwarderOptions.RetryBackoffCap is %s, above the %s ceiling (the retry horizon itself); a longer one cannot produce even a second attempt", backoffCap, MaxRetryBackoffCap)
	}

	// THE CEILING, ENFORCED AT CONSTRUCTION RATHER THAN DOCUMENTED.
	//
	// A forwarder whose last attempt could leave later than idem.PeerOutageBudget
	// after the local send silently converts that budget term into a
	// duplicate-delivery path: the receiving bus's applied-key record for the
	// original attempt may already have aged out of the retention window derived
	// from it, so the retry is applied as a NEW operation and the message is
	// delivered twice. That is a data-correctness defect produced by a
	// configuration knob, which is exactly the kind that must fail loudly at
	// startup rather than once, in production, fifty hours later.
	if horizon+timeout > RetryHorizonCeiling {
		return nil, fmt.Errorf(
			"relay: RetryHorizon (%s) + Timeout (%s) = %s exceeds the %s ceiling (idem.PeerOutageBudget). "+
				"The applied-key retention window in internal/idem/retention.go is DERIVED from that budget, so a retry issued "+
				"beyond it can land after the receiving bus has forgotten the key and be applied a SECOND time (invariant 10)",
			horizon, timeout, horizon+timeout, RetryHorizonCeiling)
	}

	log := opts.Logger
	if log == nil {
		log = logging.New(io.Discard, logging.LevelError)
	}
	// THE OUTBOX MUST RETAIN A PENDING JOB AT LEAST AS LONG AS THIS FORWARDER
	// WILL RETRY IT, AND THAT IS CHECKED RATHER THAN ASSUMED.
	//
	// The defaults satisfy it by derivation — the outbox keeps a pending record
	// for RetryHorizonCeiling and this forwarder stops at ceiling minus one
	// timeout — but OutboxOptions.RetryHorizon is configurable, and a shorter one
	// produces a silent, specific defect: the outbox sweeps a job the forwarder
	// still holds live, so the delivery goes out and its SETTLE then fails with
	// ErrOutboxUnknownJob. The message is sent with no durable record that it
	// was, which is the one state this whole task exists to remove.
	if opts.Outbox != nil && horizon+timeout > opts.Outbox.retryHorizon {
		return nil, fmt.Errorf(
			"relay: RetryHorizon (%s) + Timeout (%s) = %s exceeds the outbox's own pending-record horizon (%s). "+
				"The outbox would sweep a job while this forwarder was still retrying it, so the message would be sent and its settlement refused as an unknown job",
			horizon, timeout, horizon+timeout, opts.Outbox.retryHorizon)
	}

	if opts.Outbox == nil {
		// SAID AT CONSTRUCTION, ONCE, RATHER THAN AT EVERY LOSS. A forwarder with
		// no outbox is the RELAY-4 forwarder, and everything it queues dies with
		// the process — see the Forwarder doc. That is a legitimate
		// configuration, and it is also exactly the mis-wiring that would
		// otherwise be invisible until a crash lost traffic nobody was told
		// about.
		log.Warn("relay forwarder has NO durable outbox: cross-bus delivery from this bus is BEST EFFORT — anything queued is lost on a crash and dropped when a peer's queue fills",
			"local_bus", opts.BusID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Forwarder{
		busID:          opts.BusID,
		reg:            opts.Registry,
		client:         opts.Client,
		peerBaseURL:    opts.PeerBaseURL,
		depth:          depth,
		timeout:        timeout,
		retryHorizon:   horizon,
		backoffBase:    backoffBase,
		backoffCap:     backoffCap,
		log:            log,
		outbox:         opts.Outbox,
		recoverMessage: opts.RecoverMessage,
		ctx:            ctx,
		cancel:         cancel,
		stopping:       make(chan struct{}),
		queues:         make(map[string]chan relayJob),
		rand:           rand.New(rand.NewSource(cryptoSeed())),
	}, nil
}

// cryptoSeed draws a seed from crypto/rand so two processes do not share a
// jitter schedule. It falls back to the wall clock rather than failing
// construction: a forwarder that will not start is a worse outcome than one
// whose jitter is merely time-seeded, and crypto/rand does not fail in practice.
func cryptoSeed() int64 {
	n, err := crand.Int(crand.Reader, big.NewInt(1<<62))
	if err != nil {
		return time.Now().UnixNano()
	}
	return n.Int64()
}

// now is the clock.
func (f *Forwarder) now() time.Time { return time.Now() }

// backoffWindow is the FULL-JITTER window for the given retry attempt: the base
// doubled once per previous attempt, capped.
//
// The doubling stops AT the cap rather than overshooting it, which is not a
// micro-optimisation: with an operator-supplied base near the top of the
// int64-nanosecond range, an unguarded `w *= 2` OVERFLOWS TO A NEGATIVE
// DURATION, jitter then returns 0, and the loop spins with no sleep at all
// until the horizon runs out. NewForwarder also bounds the base, so this is the
// second of two defences rather than the only one.
func (f *Forwarder) backoffWindow(attempt int) time.Duration {
	w := f.backoffBase
	for i := 0; i < attempt; i++ {
		if w >= f.backoffCap {
			break
		}
		w *= 2
	}
	if w > f.backoffCap || w <= 0 {
		w = f.backoffCap
	}
	return w
}

// jitter draws the actual sleep from a window: a uniform draw from [0, window).
//
// # WHY THIS DOES NOT USE THE GLOBAL math/rand, WHICH IS THE OBVIOUS CHOICE
//
// go.mod pins go 1.19, where the global source is seeded with 1 unless
// GODEBUG=randautoseed=1 — so `rand.Int63n` produces the SAME SEQUENCE IN EVERY
// PROCESS, verified by running an identical probe three times and getting
// identical draws. That defeats the entire purpose. Full jitter exists to
// decorrelate retries, and the correlation that matters here is ACROSS BUSES:
// one serial worker per peer means there is little to decorrelate inside a
// single process, while a federation coming back from a shared outage — or a
// rolling restart — is exactly a set of separate processes that must not all
// probe the same peer at the same instant. A fixed seed makes them do precisely
// that.
//
// So each Forwarder holds its OWN source, seeded from crypto/rand, with its own
// mutex because rand.Rand is not safe for concurrent use and the peer workers
// are concurrent. (The same pattern, for the same reason, as
// client/transport.go's backoff.)
func (f *Forwarder) jitter(window time.Duration) time.Duration {
	if f.jitterFn != nil {
		return f.jitterFn(window)
	}
	if window <= 0 {
		return 0
	}
	f.randMu.Lock()
	defer f.randMu.Unlock()
	return time.Duration(f.rand.Int63n(int64(window)))
}

// sleep waits d, and reports whether it completed. It returns false the moment
// Close is called or the forwarder's context is cancelled, so shutdown never
// waits out a backoff.
func (f *Forwarder) sleep(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-f.stopping:
		return false
	case <-f.ctx.Done():
		return false
	}
}

// Enqueue offers m to every peer that should receive it and returns how many
// outbound copies were accepted onto a queue.
//
// IT NEVER BLOCKS. Every queue send is a non-blocking select with a default
// arm; a full queue is a counted, logged DROP rather than back-pressure onto
// the caller. That is the structural half of "a slow peer never slows a local
// send" — see the Forwarder doc.
//
// It also never returns an error a caller would surface to a local sender: the
// only error is ErrForwarderClosed, which is a shutdown/lifecycle condition on
// THIS bus. A message with no route, a message that only loops, and a message
// dropped by a full queue all return (0, nil), because none of them is a
// failure of the send that produced them.
//
// The outbound envelope is built ONCE via m.Forward(busID) — our hop appended
// exactly once, every other field verbatim (PROTOCOL.md §8.5) — and shared by
// every target, because the path a copy carries depends on where it has BEEN,
// not on where it is going next.
//
// # THE DURABLE RECORD IS WRITTEN BEFORE THE OFFER, AND THAT ORDER IS THE TASK
//
// With an Outbox configured, each target's `pending` record is written and
// FSYNCED before the job is offered to that peer's queue. The reverse order
// would leave a window — small, and hit exactly when it hurts — in which a job
// is being sent to a peer with nothing on disk saying we owe it, so a crash
// mid-flight loses a hop that the peer may or may not have taken. Writing first
// means the durable set is always a SUPERSET of what is in flight, which is the
// direction that costs a duplicate (which invariant 10's applied-key check
// absorbs) rather than a loss (which nothing absorbs).
//
// # SO Enqueue NOW COSTS TWO FSYNCS PER TARGET PEER, ON THE CALLER'S GOROUTINE
//
// Stated plainly, and stated EXACTLY, because it is a real cost the caller
// cannot see: one Outbox.Enqueue is one wal.Log.Write, and that is a PREPARE and
// a COMMIT with an unconditional fsync each (internal/wal/writer.go). So a
// broadcast to N peers performs 2N fsyncs, serially, before returning. (An
// earlier version of this paragraph said N and was corrected by the security
// gate; a cost comment that is wrong by a factor of two is worse than none,
// because it is what the next person capacity-plans against.)
//
// It does not violate "a slow or dead peer must never make a local send slow" —
// no peer is involved in an fsync, and the queue send is still non-blocking —
// but it does mean a local send now waits on local disk proportionally to the
// federation size. Batching all N jobs into one WAL entry is the obvious
// remedy; it changes the record shape RELAY-15 fixed (one record = one job) and
// is deliberately NOT done here.
func (f *Forwarder) Enqueue(m RelayedMessage) (int, error) {
	targets := f.targets(m)
	if len(targets) == 0 {
		return 0, nil
	}

	req, err := m.Forward(f.busID)
	if err != nil {
		// The only ways this fails are that we are already on the message's own
		// path (a loop the ingress check should have caught) or that the path is
		// at its cap. Neither is the local sender's problem.
		if errors.Is(err, ErrRelayLoop) {
			f.dropLoop.Add(1)
		} else {
			f.dropNoRoute.Add(1)
		}
		f.log.Warn("relay forward abandoned before it was queued",
			"local_bus", f.busID,
			"origin_message_id", m.OriginMessageID,
			"err", err.Error(),
		)
		return 0, nil
	}

	// FORWARDING BEFORE Resume IS A WIRING BUG, AND IT IS SAID OUT LOUD ONCE.
	//
	// Resume's doc requires it to run before this forwarder is put in service. If
	// it does not, a live Enqueue for a (peer, message) that is STILL PENDING
	// from the last run gets handed the existing record by Outbox.Enqueue's
	// idempotent path and offers a second copy of a job Resume is about to offer
	// too — one duplicate relay, plus an ErrOutboxSettled at Error when the
	// second settle lands.
	//
	// THE SECURITY GATE PROPOSED REFUSING THE ENQUEUE INSTEAD, AND THAT IS
	// DELIBERATELY NOT DONE. A refusal converts a startup-ordering mistake into a
	// TOTAL forwarding outage, while the thing it prevents is a duplicate that
	// invariant 10's applied-key check is designed to absorb. Same trade, and the
	// same direction, as resumeJob's no-route arm: recoverable beats
	// irreversible. What is not acceptable is it being INVISIBLE, so it is a
	// Warn, and it fires once rather than per message because a per-send line
	// would bury itself.
	if f.outbox != nil {
		f.mu.Lock()
		warn := !f.resumed && !f.warnedUnresumed
		if warn {
			f.warnedUnresumed = true
		}
		f.mu.Unlock()
		if warn {
			f.log.Warn("relay forwarding started BEFORE Resume: deliveries still owed from the last run have not been re-offered yet, so a message already pending for a peer may be sent twice. Call Resume after the peer roster is restored and before serving traffic",
				"local_bus", f.busID)
		}
	}

	queued := 0
	for _, peerBusID := range targets {
		baseURL, ok := f.peerBaseURL(peerBusID)
		if !ok || baseURL == "" {
			f.dropNoRoute.Add(1)
			f.log.Warn("relay target has no known base URL",
				"local_bus", f.busID, "peer_bus", peerBusID, "origin_message_id", m.OriginMessageID)
			continue
		}
		job := relayJob{peerBusID: peerBusID, baseURL: baseURL, req: req, enqueuedAt: f.now()}
		if f.outbox != nil {
			// THE PEER ID IS STORED AS THE REGISTRY SPELLS IT, NOT FOLDED — AND
			// THAT DECLINES WHAT DeriveJobID'S DOC ASKS RELAY-19 TO DO. The reason
			// is mechanical and was found by trying it: Registry.PeerBaseURL and
			// Registry.Route both require an EXACT match on the stored spelling
			// (registry.go:506 and :624 compare st.busID, having only KEYED the map
			// on the folded form). A record holding a folded id therefore cannot
			// resolve its own peer's address at Resume — every job for a
			// mixed-case peer would come back from a crash unroutable and be
			// abandoned. Folding here would trade a duplicate that cannot happen
			// for a loss that would.
			//
			// The duplicate DeriveJobID warns about needs two case-variant
			// spellings of one peer to reach this line, and they cannot: targets()
			// takes every id from the Registry, which is keyed on the folded form
			// and holds exactly ONE spelling per peer at a time. The fix belongs
			// where the ambiguity is — the Registry canonicalising the id it
			// stores, so every consumer sees one spelling — and is reported as a
			// follow-up rather than patched around here.
			rec, err := f.outbox.Enqueue(OutboxJob{
				PeerBusID:       peerBusID,
				OriginMessageID: m.OriginMessageID,
				Size:            len(m.Body),
				ContentSHA256:   m.ContentSHA256,
			})
			if err != nil {
				// NOT OFFERED — but WHY it was not offered decides whether this is
				// a loss, and two of the errors here are not losses at all. Filing
				// them under NotDurable would raise a "this message is gone" alarm
				// for a job that is delivered or is being written right now, and a
				// counter that cries wolf is a counter an operator learns to
				// ignore.
				switch {
				case errors.Is(err, ErrOutboxSettled):
					// ALREADY DELIVERED OR ABANDONED for this peer. The outbox
					// refusing to resurrect it is the anti-duplicate rule working
					// exactly as designed, so this is a SUPPRESSION, not a drop.
					f.log.Debug("relay delivery not re-queued: this peer's copy of the message is already settled in the durable outbox",
						"local_bus", f.busID, "peer_bus", peerBusID, "origin_message_id", m.OriginMessageID, "err", err.Error())
				case errors.Is(err, ErrOutboxInFlight):
					// A CONCURRENT Enqueue of the SAME job is mid-fsync. outbox.go
					// documents this as RETRYABLE and requires callers to treat it
					// so; here the retry is implicit and better than a resend,
					// because the enqueue already in flight will offer this exact
					// job when it lands. Nothing is lost and a restart would find
					// the record.
					f.log.Warn("relay delivery not queued here: an enqueue of the same job is already being written, and that one will queue it",
						"local_bus", f.busID, "peer_bus", peerBusID, "origin_message_id", m.OriginMessageID, "err", err.Error())
				default:
					// A GENUINE LOSS: the capacity bound, or the durable write
					// failed. A job with no durable record cannot be settled, so
					// sending it anyway would put an unsettleable delivery on the
					// wire and make every terminal outcome log ErrOutboxUnknownJob
					// at ERROR — burying the one line that means a real lost hop
					// under a stream that does not. Refusing is also what the
					// capacity bound is FOR: it says this bus already owes more
					// than it can track.
					f.dropNotDurable.Add(1)
					f.log.Warn("relay delivery could NOT be recorded durably, so it was never queued; this message will not reach the peer and no restart brings it back",
						"local_bus", f.busID,
						"peer_bus", peerBusID,
						"origin_message_id", m.OriginMessageID,
						"err", err.Error(),
						"dropped_not_durable_total", f.dropNotDurable.Load(),
					)
				}
				continue
			}
			job.jobID = rec.JobID
			// THE DURABLE ANCHOR WINS OVER THE LOCAL CLOCK READING. deliver's
			// deadline is enqueuedAt + retryHorizon, and after a restart Resume
			// can only supply the RECORD's anchor — so using it here too makes the
			// horizon mean the same thing before and after a crash. It is also the
			// subtraction outbox.go asks RELAY-19 to own: the record is retained
			// for the full RetryHorizonCeiling, while the forwarder issues attempts
			// only inside its own (ceiling - timeout) horizon measured from this
			// same instant, so the last outbound byte cannot leave after the
			// budget.
			job.enqueuedAt = rec.EnqueuedAt
		}
		accepted, err := f.offer(job)
		if err != nil {
			// ErrForwarderClosed. The pending record is deliberately NOT settled:
			// a shutdown is not a verdict on the message, and leaving it owed is
			// precisely what makes it survive to the next start.
			return queued, err
		}
		if accepted {
			f.queued.Add(1)
			queued++
		} else {
			// A FULL QUEUE IS NO LONGER A LOSS WHEN THE JOB IS DURABLE. The record
			// stays PENDING and Resume re-offers it on the next start — an
			// in-memory bound is a reason to send later, not a licence to destroy
			// work already accepted. Blocking here would still be back-pressure
			// from a dead peer onto a local send, so the offer stays non-blocking.
			//
			// At default settings this arm is nearly unreachable WITH an outbox:
			// the per-peer pending quota and the queue depth are both
			// DefaultQueueDepth, so the outbox refuses (NotDurable) before the
			// queue fills. It is reachable for a caller that configures them
			// apart.
			f.dropFull.Add(1)
			if f.outbox != nil {
				f.log.Warn("relay queue full; the message is NOT sent now, but it remains durably owed and will be re-offered after the next restart",
					"local_bus", f.busID,
					"peer_bus", peerBusID,
					"origin_message_id", m.OriginMessageID,
					"job_id", job.jobID,
					"queue_depth", f.depth,
					"dropped_full_total", f.dropFull.Load(),
				)
			} else {
				f.log.Warn("relay queue full; message dropped for this peer (this forwarder has no durable outbox, so the outbound queue is process memory and cross-bus delivery is BEST EFFORT)",
					"local_bus", f.busID,
					"peer_bus", peerBusID,
					"origin_message_id", m.OriginMessageID,
					"queue_depth", f.depth,
					"dropped_full_total", f.dropFull.Load(),
				)
			}
		}
	}
	return queued, nil
}

// settle writes the TERMINAL outbox record for a job that has reached an
// outcome, and is a no-op for a forwarder with no outbox.
//
// A FAILURE HERE IS LOGGED AT ERROR AND SWALLOWED, because there is nothing
// left to fail: the delivery attempt has already happened.
//
// WHAT IT COSTS DEPENDS ON WHICH FAILURE IT IS, AND THE LOG LINE MUST NOT PICK
// ONE. An earlier version said "the job stays pending and will be re-sent",
// which is true only for the transport-level failures:
//
//   - the durable write failed, or the job is transiently ErrOutboxInFlight —
//     the record is still PENDING, so the next start re-offers it and the peer
//     absorbs a duplicate (invariant 10). Safe direction, not free.
//   - ErrOutboxSettled / ErrOutboxUnknownJob — the record is already terminal,
//     or is not there at all (swept past its horizon, or dropped by the skew
//     guard). NOTHING will be re-sent, and the outcome this call was reporting
//     is simply absent from the durable trail.
//
// Both are worth an ERROR and neither may be described as the other, so the
// line names the state it tried to write and quotes the error rather than
// predicting the consequence.
func (f *Forwarder) settle(job relayJob, state OutboxState, reason string) {
	if f.outbox == nil || job.jobID == "" {
		return
	}
	if _, err := f.outbox.Settle(job.jobID, state, reason); err != nil {
		f.log.Error("could not record the OUTCOME of a relay delivery durably; the durable trail for this job is now incomplete — if its record is still pending the next start re-sends it (a duplicate the peer absorbs), and if it is settled or gone this outcome is simply unrecorded",
			"local_bus", f.busID,
			"peer_bus", job.peerBusID,
			"origin_message_id", job.req.MessageID,
			"job_id", job.jobID,
			"state", state.String(),
			"err", err.Error(),
		)
	}
}

// targets resolves which peers should receive m, applying the egress split
// horizon (NextHopAllowed) to every candidate.
//
// A broadcast fans out to every peer not already on the path; a directed
// message goes to the peer owning each recipient, deduplicated, because two
// recipients on one peer are one outbound copy.
func (f *Forwarder) targets(m RelayedMessage) []string {
	if m.Broadcast {
		// BroadcastTargets already applies the split horizon.
		return f.reg.BroadcastTargets(m.BusPath)
	}
	out := make([]string, 0, len(m.Recipients))
	seen := make(map[string]struct{}, len(m.Recipients))
	for _, r := range m.Recipients {
		peerBusID, ok := f.reg.Route(r)
		if !ok {
			// Not a peer's agent: either ours (a local delivery, already done)
			// or nobody we know. Counted so an operator can see a bus routing
			// into the void.
			f.dropNoRoute.Add(1)
			continue
		}
		if !NextHopAllowed(m.BusPath, peerBusID) {
			f.dropLoop.Add(1)
			continue
		}
		if _, dup := seen[peerBusID]; dup {
			continue
		}
		seen[peerBusID] = struct{}{}
		out = append(out, peerBusID)
	}
	return out
}

// offer puts one job on its peer's queue, starting that peer's goroutine on
// first use, and reports whether the queue had room.
//
// # THE SEND HAPPENS UNDER f.mu, AND THAT IS NOT INCIDENTAL
//
// It is what makes Enqueue safe against a concurrent Close. Close closes every
// queue channel while holding f.mu, so a design that looked up the channel under
// the lock and then SENT on it after releasing the lock would have a window in
// which Close closes the channel between the two — and a send on a closed
// channel is an unrecoverable PANIC, taking down a server for the crime of
// shutting down while a message was being forwarded. Holding the lock across the
// send closes that window entirely.
//
// Holding a mutex across a channel send is normally the thing to avoid, and it
// is safe HERE for one specific reason: the send is NON-BLOCKING (a select with
// a default arm), so the critical section is bounded by a queue-slot check and
// can never wait on a peer, on the network, or on a worker goroutine. The
// property that makes Enqueue non-blocking is the same property that makes it
// safe to hold the lock. If anyone ever removes the default arm, this becomes a
// deadlock — so do not remove it.
func (f *Forwarder) offer(job relayJob) (bool, error) {
	key := strings.ToLower(job.peerBusID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false, ErrForwarderClosed
	}
	ch, ok := f.queues[key]
	if !ok {
		ch = make(chan relayJob, f.depth)
		f.queues[key] = ch
		f.wg.Add(1)
		f.workers.Add(1)
		go f.serve(job.peerBusID, ch)
	}
	select {
	case ch <- job:
		return true, nil
	default:
		return false, nil
	}
}

// serve is one peer's goroutine. It drains its own queue and nothing else, so a
// peer that hangs delays only its own traffic.
func (f *Forwarder) serve(peerBusID string, ch chan relayJob) {
	defer f.wg.Done()
	defer f.workers.Add(-1)
	for job := range ch {
		select {
		case <-f.ctx.Done():
			// Close has given up waiting; abandon the rest of the queue rather
			// than outliving the shutdown.
			//
			// NOTHING IS SETTLED HERE, DELIBERATELY. This is one of the four
			// shutdown paths that used to be uncounted silent loss; with an outbox
			// every job still on this queue keeps its PENDING record, so the next
			// start re-offers it. Writing an abandonment would convert a
			// recoverable shutdown into the permanent loss it used to be.
			f.log.Warn("relay peer worker abandoned its queue at shutdown; every abandoned job keeps its durable outbox record and is re-offered after the next start (a forwarder with no outbox loses them)",
				"local_bus", f.busID, "peer_bus", peerBusID, "abandoned", len(ch)+1, "durable", f.outbox != nil)
			return
		default:
		}
		f.deliver(job, ch)
	}
	f.log.Debug("relay peer worker stopped", "local_bus", f.busID, "peer_bus", peerBusID)
}

// deliver runs one job to a settled outcome: delivered, permanently refused, or
// out of retry horizon (RELAY-4).
//
// # THE DEADLINE IS ANCHORED ON enqueuedAt, NOT ON THE FIRST ATTEMPT
//
// See the RetryHorizonCeiling derivation: per-peer queues drain serially, so a
// first-attempt anchor would let horizons stack behind a dead peer's head-of-line
// job and blow through idem.PeerOutageBudget. A job whose deadline passed while
// it waited in the queue is dropped here, before its first attempt.
func (f *Forwarder) deliver(job relayJob, queue chan relayJob) {
	deadline := job.enqueuedAt.Add(f.retryHorizon)
	if !f.now().Before(deadline) {
		f.dropExpired.Add(1)
		f.settle(job, OutboxAbandoned, "the retry horizon elapsed while this job waited in the peer's queue; retrying past idem.PeerOutageBudget would be applied by the peer as a NEW message")
		f.log.Warn("relay job passed its retry horizon while queued; dropped without being attempted (the peer has been unreachable longer than the outage budget)",
			"local_bus", f.busID,
			"peer_bus", job.peerBusID,
			"origin_message_id", job.req.MessageID,
			"retry_horizon", f.retryHorizon.String(),
		)
		return
	}

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			f.retried.Add(1)
		}
		if settled, retriable := f.attempt(&job, attempt); settled {
			return
		} else if !retriable {
			// A cancelled forwarder context is SHUTDOWN, not a verdict on this
			// message, and must not be filed as one. Counting it in
			// Dropped.Permanent would tell an operator to go and fix a message
			// or a peering after every ordinary restart — the counter is only
			// worth having if it means what it says.
			if f.ctx.Err() != nil {
				// SHUTDOWN, AND THEREFORE NOT SETTLED — the second of the four
				// paths that used to lose a message silently. The pending record
				// stands and the next start re-offers this job.
				f.log.Warn("relay forward abandoned at shutdown; the job keeps its durable outbox record and is re-offered after the next start",
					"local_bus", f.busID,
					"peer_bus", job.peerBusID,
					"origin_message_id", job.req.MessageID,
					"attempts", attempt+1,
					"durable", f.outbox != nil,
				)
				return
			}
			f.dropPermanent.Add(1)
			f.settle(job, OutboxAbandoned, "the peer refused permanently, so resending identical bytes cannot change the answer: "+job.lastErr)
			f.log.Warn("relay forward permanently refused; NOT retried, because resending identical bytes cannot change this answer",
				"local_bus", f.busID,
				"peer_bus", job.peerBusID,
				"origin_message_id", job.req.MessageID,
				"attempts", attempt+1,
			)
			return
		}

		wait := f.jitter(f.backoffWindow(attempt))
		if !f.now().Add(wait).Before(deadline) {
			f.dropExpired.Add(1)
			// SETTLED abandoned, NOT left pending. This is the one loss a durable
			// outbox deliberately does not prevent: keeping the job would mean
			// retrying it past idem.PeerOutageBudget, where the peer has forgotten
			// the applied key and takes the retry as a NEW message. A duplicate
			// delivery is worse than a recorded loss, so the horizon still wins and
			// the record says exactly why (invariant 6).
			f.settle(job, OutboxAbandoned, "the retry horizon was exhausted against an unreachable peer; retrying past idem.PeerOutageBudget would be applied by the peer as a NEW message: "+job.lastErr)
			f.log.Warn("relay forward gave up: the retry horizon is exhausted and the message is LOST — it is settled ABANDONED rather than kept, because a retry past idem.PeerOutageBudget is a DUPLICATE delivery",
				"local_bus", f.busID,
				"peer_bus", job.peerBusID,
				"origin_message_id", job.req.MessageID,
				"attempts", attempt+1,
				"retry_horizon", f.retryHorizon.String(),
			)
			return
		}
		// HEAD-OF-LINE YIELD. A retrying job holds this peer's only worker, so
		// while it retries, everything behind it waits — and once the queue is
		// FULL, everything behind it is DROPPED at Enqueue. At that point
		// continuing to retry this one message costs a message per new send, and
		// it is no longer a trade between "retry" and "give up": it is a trade
		// between one message and all the others.
		//
		// The case this actually closes is a POISON MESSAGE ON A HEALTHY PEER,
		// which the "a dead peer only hurts its own queue" argument does not
		// cover. relayhttp.go maps every unclassified AcceptRelay failure to 503,
		// and 503 is retriable, so ONE deterministically-failing envelope would
		// otherwise hold the head for the whole horizon against a peer that is
		// answering everything else perfectly well.
		//
		// Below the full mark this does nothing at all, which is the common case
		// RELAY-4 was written for: a peer down with light traffic behind it gets
		// the full horizon.
		if len(queue) == cap(queue) {
			f.dropYielded.Add(1)
			// SETTLED abandoned. The yield exists to free the head so the rest of
			// the queue moves; leaving this job pending would put it straight back
			// at the head of the same full queue on the next start, which is the
			// poison-message loop the yield was written to break.
			f.settle(job, OutboxAbandoned, "yielded the head of a FULL queue after a retriable failure, so the rest of the peer's queue could move: "+job.lastErr)
			f.log.Warn("relay forward yielded the head of a FULL queue after a retriable failure; this message is dropped so the rest of the peer's queue can move (one message lost instead of every message behind it)",
				"local_bus", f.busID,
				"peer_bus", job.peerBusID,
				"origin_message_id", job.req.MessageID,
				"attempts", attempt+1,
				"queue_depth", f.depth,
			)
			return
		}
		if !f.sleep(wait) {
			// Close, or a cancelled forwarder context. Abandoning the retry is
			// the correct shutdown behaviour: waiting out a dead peer's backoff
			// would make every shutdown as slow as the worst peer.
			//
			// The third un-settled shutdown path. The job is mid-retry, so it is
			// exactly the case a durable outbox exists for: its pending record
			// stands and the next start picks it up where this one left off.
			f.log.Warn("relay forward abandoned mid-backoff at shutdown; the job keeps its durable outbox record and is re-offered after the next start",
				"local_bus", f.busID, "peer_bus", job.peerBusID, "origin_message_id", job.req.MessageID,
				"durable", f.outbox != nil)
			return
		}
	}
}

// attempt performs ONE outbound relay attempt and classifies the result.
//
// settled means the job is finished — delivered, duplicate-suppressed, or
// answered with a settled loop drop. When settled is false, retriable says
// whether resending the identical bytes could ever produce a different answer.
//
// The per-attempt context descends from the Forwarder's own, so Close can abort
// an in-flight request; without that, a peer that accepts a connection and
// never answers would keep a goroutine alive past Close and the -race build
// would (rightly) report a goroutine leak.
func (f *Forwarder) attempt(job *relayJob, attempt int) (settled, retriable bool) {
	// THE ADDRESS IS RE-RESOLVED ON EVERY ATTEMPT, NOT FROZEN AT ENQUEUE.
	//
	// Retry stretched the window between the routing decision and the last POST
	// from one attempt (~30s) to the whole horizon (~24h), and an address held
	// across that window is a REVOCATION HOLE: after RemovePeer de-peers a
	// compromised bus, or SetPeerBaseURL moves a peer off a hijacked address,
	// a frozen job would keep posting the message — sender, recipients and body
	// — at the old address for the rest of the horizon. Re-resolving makes an
	// operator's de-peering take effect on the NEXT attempt instead of the next
	// day, and a peer that is gone abandons the job rather than being retried at
	// an address nobody vouches for any more.
	baseURL, ok := f.peerBaseURL(job.peerBusID)
	if !ok || baseURL == "" {
		f.dropNoRoute.Add(1)
		// SETTLED abandoned, and this one is a SECURITY property as much as a
		// bookkeeping one: the address was re-resolved precisely so that
		// de-peering takes effect on the next attempt. A job left pending here
		// would be re-offered at every restart and keep trying to post the
		// message to a bus an operator has deliberately removed, for the whole
		// horizon. De-peered means we do not owe it any more.
		f.settle(*job, OutboxAbandoned, "the destination peer is no longer known, or has no base URL: it was de-peered, or its address was moved, after this message was queued")
		f.log.Warn("relay forward abandoned: the peer is no longer known, or has no base URL (de-peered, or its address was moved, since this message was queued)",
			"local_bus", f.busID,
			"peer_bus", job.peerBusID,
			"origin_message_id", job.req.MessageID,
			"attempt", attempt+1,
		)
		return true, false
	}
	if baseURL != job.baseURL {
		f.log.Warn("relay peer address changed while a message was queued; the NEW address is used, and the old one is not retried",
			"local_bus", f.busID,
			"peer_bus", job.peerBusID,
			"origin_message_id", job.req.MessageID,
		)
		// Recorded on the job so the line is logged ONCE PER CHANGE rather than
		// once per attempt. Without this the job's address stays stale forever
		// and a single move emits an identical line on every remaining attempt —
		// hundreds of them over a full horizon, which buries the one that
		// mattered.
		job.baseURL = baseURL
	}

	ctx, cancel := context.WithTimeout(f.ctx, f.timeout)
	defer cancel()

	resp, err := f.client.Relay(ctx, baseURL, job.req)
	if err != nil {
		f.failed.Add(1)
		retriable = f.retriable(err)
		// Carried so the DURABLE abandonment deliver may write can say what went
		// wrong. It is untrusted text and is bounded on the way into the record by
		// sanitiseOutboxReason, never here.
		job.lastErr = err.Error()
		f.log.Warn("relay forward failed",
			"local_bus", f.busID,
			"peer_bus", job.peerBusID,
			"origin_message_id", job.req.MessageID,
			"attempt", attempt+1,
			"retriable", retriable,
			"err", err.Error(),
		)
		return false, retriable
	}
	f.sent.Add(1)
	if !resp.Accepted && resp.DroppedReason == DropLoop {
		// A settled, expected outcome in a cyclic topology, NOT a failure. It
		// is counted as a loop drop so the mesh's shape is visible, and it is
		// explicitly not retried — this is precisely why relayhttp.go answers a
		// loop with 200 rather than an error status.
		f.dropLoop.Add(1)
		f.log.Debug("peer dropped a relayed message as a loop",
			"local_bus", f.busID, "peer_bus", job.peerBusID, "origin_message_id", job.req.MessageID)
	}
	// SETTLED delivered — and that covers the loop drop above as well as an
	// acceptance. OutboxDelivered means "the peer answered finally, and there is
	// nothing left to send", which is exactly what a 200 with a loop drop is: the
	// peer looked at the message and settled it (relayhttp.go answers 200 for
	// precisely this reason). Recording it as ABANDONED would file the expected
	// steady state of a cyclic topology as a message this bus failed to deliver,
	// and would put a Warn on every lap of every cycle.
	f.settle(*job, OutboxDelivered, "")
	return true, false
}

// retriable decides whether a failed attempt is worth repeating.
//
// The default is YES, and that direction is deliberate: an unrecognised failure
// is almost always a transport fault (dial refused, TLS handshake, connection
// reset, read timeout), which is exactly RELAY-4's case. The NOs are the cases
// where repeating is provably useless or actively harmful:
//
//   - The forwarder is shutting down (f.ctx cancelled). Not the peer's fault and
//     not worth a retry we are about to abandon anyway.
//   - A refusal whose status says "never" — see PeerRefusedError.Retriable. This
//     is the arm that stops the retry loop becoming the traffic amplifier
//     relayhttp.go's status argument warns about.
//   - An envelope this bus could not encode, or that exceeds MaxRelayBytes. The
//     bytes will not shrink on a second try.
func (f *Forwarder) retriable(err error) bool {
	if f.ctx.Err() != nil {
		return false
	}
	var refused *PeerRefusedError
	if errors.As(err, &refused) {
		return refused.Retriable()
	}
	if errors.Is(err, ErrPayloadTooLarge) {
		return false
	}
	return true
}

// ErrForwarderResumed reports a SECOND Resume on one Forwarder.
//
// It is an error rather than a no-op because the two readings of a repeated
// Resume are opposite and only the caller knows which it meant: a harmless
// re-run of startup code, or a bug that is about to send every still-pending
// job a second time. Refusing loudly picks neither and reports the fact.
var ErrForwarderResumed = errors.New("relay: forwarder has already resumed its durable outbox")

// ErrForwarderNotDurable reports Resume on a forwarder with no Outbox.
var ErrForwarderNotDurable = errors.New("relay: forwarder has no durable outbox to resume from")

// Resume re-offers the durable pending set: every delivery this bus still owes
// a peer, recovered from the outbox and put back on its peer's queue.
//
// It returns how many jobs were re-offered. IT MUST BE CALLED AFTER THE
// FORWARDER IS CONSTRUCTED AND BEFORE IT IS PUT IN SERVICE, and the ordering is
// the task's own definition of done — the pending set is rebuilt by the WAL
// replay at Open, so it exists before this Forwarder does, and re-offering it is
// what turns a recovered record back into work in progress.
//
// # AND AFTER THE PEER ROSTER IS RESTORED. THIS IS A PRECONDITION, NOT A HINT
//
// Every recovered job is resolved through PeerBaseURL. A Resume that runs
// before the roster is loaded therefore sees EVERY peer as unknown — so the
// wiring order is part of the contract, and the failure it produces would be
// silent, total and at startup. The no-route arm below is written to survive
// exactly that mistake (it leaves such jobs OWED rather than abandoning them),
// but relying on that recovery instead of the ordering would mean a bus that
// re-offers nothing on every boot and says so only at Warn.
//
// # EXACTLY ONCE, AND EXACTLY WHERE THAT PROPERTY COMES FROM
//
// Not from this function alone. Three mechanisms compose, and each covers a
// window the others do not:
//
//  1. WITHIN A PROCESS, the resumed flag. A second Resume is refused, so a job
//     recovered here can be offered at most once per process lifetime.
//  2. ACROSS RESTARTS, the TOMBSTONE. A job that was delivered is settled, and
//     Outbox.Pending never returns a settled record — so the next start does not
//     see it at all. That is the mechanism that stops a recovered job being
//     re-sent forever, and it is why every terminal path in this file settles.
//  3. AGAINST A CRASH BETWEEN THE SEND AND THE SETTLE, nothing here — and
//     deliberately. That window is real: the peer took the message and this bus
//     died before writing `delivered`, so the next start re-offers it and the
//     peer sees it twice. It is absorbed by invariant 10's applied-key check at
//     the receiving bus, which is what at-least-once delivery means. Closing it
//     locally is impossible; a durable record can be written before the send or
//     after it, never atomically with it.
//
// A job that cannot be rebuilt is SETTLED ABANDONED rather than left pending:
// invariant 6 requires a message this bus will never deliver to be recorded
// specifically, and leaving it pending would re-run the same failing rebuild at
// every start while holding its peer's quota until the horizon expired.
func (f *Forwarder) Resume() (int, error) {
	if f.outbox == nil {
		return 0, ErrForwarderNotDurable
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return 0, ErrForwarderClosed
	}
	if f.resumed {
		f.mu.Unlock()
		return 0, ErrForwarderResumed
	}
	f.resumed = true
	f.mu.Unlock()

	// Pending() is ordered oldest-enqueue-first, so a restart re-offers jobs in
	// the sequence it would have sent them.
	pending := f.outbox.Pending()
	requeued := 0
	for _, rec := range pending {
		offered, err := f.resumeJob(rec)
		if offered {
			requeued++
		}
		if err != nil {
			// A CLOSE MID-RESUME STOPS THE PASS AND IS REPORTED. Continuing would
			// call offer once per remaining job for a forwarder that is shutting
			// down, and returning nil would tell the caller the pending set had
			// been resumed when most of it never was — the jobs are still owed, so
			// this is a truthfulness bug rather than a loss.
			f.log.Warn("relay outbox resume stopped early", "local_bus", f.busID,
				"pending", len(pending), "requeued", requeued, "err", err.Error())
			return requeued, err
		}
	}
	if len(pending) > 0 {
		f.log.Info("relay outbox resumed: deliveries this bus still owed at the last shutdown are back on their peers' queues",
			"local_bus", f.busID, "pending", len(pending), "requeued", requeued)
	}
	return requeued, nil
}

// resumeJob rebuilds one recovered job and offers it, reporting whether it went
// onto a queue and whether the forwarder was closed underneath it.
func (f *Forwarder) resumeJob(rec OutboxRecord) (bool, error) {
	abandon := func(reason string) (bool, error) {
		f.settle(relayJob{peerBusID: rec.PeerBusID, jobID: rec.JobID, req: RelayRequest{MessageID: rec.OriginMessageID}}, OutboxAbandoned, reason)
		f.log.Warn("a recovered relay job could NOT be resumed and is ABANDONED; this message will never reach the peer",
			"local_bus", f.busID, "peer_bus", rec.PeerBusID,
			"origin_message_id", rec.OriginMessageID, "job_id", rec.JobID, "reason", reason)
		return false, nil
	}

	m, ok, err := f.recoverMessage(rec.OriginMessageID)
	if err != nil {
		return abandon("the message this job names could not be read back from the durable store: " + err.Error())
	}
	if !ok {
		return abandon("the message this job names is no longer in the durable store, so the envelope cannot be rebuilt")
	}
	// THE REBUILD IS CHECKED, NOT ASSUMED — which is the reason the record stores
	// Size and ContentSHA256 at all (outbox.go's file comment, reason 2). A
	// message id is minted once and never reused, so a message that no longer
	// hashes to what the job was created for means the store handed back
	// something else, and sending it would relay content nobody signed off on.
	if m.ContentSHA256 != rec.ContentSHA256 || len(m.Body) != rec.Size {
		return abandon(fmt.Sprintf("the recovered message does not match the job: the job names %s (%d bytes) and the store returned %s (%d bytes)",
			rec.ContentSHA256, rec.Size, m.ContentSHA256, len(m.Body)))
	}
	// THE SPLIT HORIZON IS RE-APPLIED AT RESUME. The path is carried by the
	// message, not by the record, and a peer that has since appeared on this
	// message's traversed path is a hop we must not make — the same check
	// targets() applies on the live path, applied again because the federation
	// may have changed shape while this bus was down.
	if !NextHopAllowed(m.BusPath, rec.PeerBusID) {
		f.dropLoop.Add(1)
		return abandon("the destination peer is already on this message's traversed bus path, so forwarding it would be a loop")
	}
	req, err := m.Forward(f.busID)
	if err != nil {
		return abandon("the outbound envelope could not be rebuilt: " + err.Error())
	}
	baseURL, ok := f.peerBaseURL(rec.PeerBusID)
	if !ok || baseURL == "" {
		// LEFT PENDING, NOT ABANDONED — and this is the ONE arm of resumeJob that
		// declines to settle, which is why it is argued rather than asserted.
		//
		// attempt() abandons on the same condition, and consistency would say to
		// do it here too. The difference is WHEN: attempt runs on a bus that has
		// been up long enough to have queued the job, so "this peer is unknown"
		// means it was de-peered. Resume runs at STARTUP, where the identical
		// reading is also produced by a wiring mistake — Resume called before the
		// peer roster is loaded (see the precondition above) — and there it is
		// true of EVERY peer at once. Abandoning would then destroy the entire
		// recovered backlog, durably, at boot, from an ordering bug.
		//
		// So the fail-safe direction wins: the job stays owed, the next start with
		// the correct ordering delivers it, and a peer that really is gone costs a
		// pending slot until the horizon retires the job anyway. A recoverable
		// delay is a smaller failure than an irreversible loss.
		f.dropNoRoute.Add(1)
		f.log.Warn("a recovered relay job has no route: the peer is unknown or has no base URL, so it is NOT re-offered. It remains durably owed. If this fires for every job at startup, Resume is running before the peer roster is restored",
			"local_bus", f.busID, "peer_bus", rec.PeerBusID,
			"origin_message_id", rec.OriginMessageID, "job_id", rec.JobID)
		return false, nil
	}

	// THE DURABLE ANCHOR IS THE RECORD'S, NOT NOW. deliver measures the retry
	// horizon from enqueuedAt, so re-anchoring on the restart instant would give
	// every recovered job a FRESH full horizon — and a bus that restarted daily
	// could retry one job indefinitely, landing outside idem.PeerOutageBudget
	// where the peer applies it as a NEW message. Keeping the original anchor is
	// also what lets deliver drop a job that expired while this bus was down,
	// before it is attempted.
	job := relayJob{peerBusID: rec.PeerBusID, baseURL: baseURL, req: req, enqueuedAt: rec.EnqueuedAt, jobID: rec.JobID}
	accepted, err := f.offer(job)
	if err != nil {
		// Closed mid-Resume. Not settled: still owed, and the next start re-offers.
		// Reported so Resume stops rather than walking the rest of the backlog
		// against a forwarder that is shutting down.
		return false, err
	}
	if !accepted {
		// The pending record STANDS. See Enqueue's full-queue arm: an in-memory
		// bound is a reason to send later. Reachable here when a recovered
		// backlog for one peer exceeds the queue depth, which replay admits
		// deliberately (outbox.go's pre-quota backlog note).
		f.dropFull.Add(1)
		f.log.Warn("a recovered relay job did not fit its peer's queue; it remains durably owed and is re-offered after the next start",
			"local_bus", f.busID, "peer_bus", rec.PeerBusID,
			"origin_message_id", rec.OriginMessageID, "job_id", rec.JobID, "queue_depth", f.depth)
		return false, nil
	}
	f.queued.Add(1)
	return true, nil
}

// Stats reports the forwarder's counters.
func (f *Forwarder) Stats() ForwarderStats {
	return ForwarderStats{
		Queued:  f.queued.Load(),
		Sent:    f.sent.Load(),
		Failed:  f.failed.Load(),
		Retried: f.retried.Load(),
		Workers: f.workers.Load(),
		Dropped: DropCounts{
			Full:       f.dropFull.Load(),
			Loop:       f.dropLoop.Load(),
			NoRoute:    f.dropNoRoute.Load(),
			Expired:    f.dropExpired.Load(),
			Permanent:  f.dropPermanent.Load(),
			Yielded:    f.dropYielded.Load(),
			NotDurable: f.dropNotDurable.Load(),
		},
	}
}

// Close stops every peer goroutine, draining what it can within ctx.
//
// NO GOROUTINE OUTLIVES Close. If ctx expires first, in-flight requests are
// CANCELLED (which is what makes the guarantee hold against a peer that has
// accepted a connection and gone silent) and Close still waits for the
// goroutines to finish unwinding before returning ctx.Err(). Returning early
// while a goroutine was still running would leak it into the next test — and,
// in production, past the shutdown that was supposed to have ended it.
//
// Close is idempotent; a second call returns nil.
func (f *Forwarder) Close(ctx context.Context) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	// Closed BEFORE the queues so a worker parked in a backoff wakes at once.
	// Without it a graceful Close would wait up to backoffCap per peer that
	// happened to be retrying — a shutdown as slow as the worst peer.
	close(f.stopping)
	for _, ch := range f.queues {
		close(ch)
	}
	f.mu.Unlock()

	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		f.cancel()
		return nil
	case <-ctx.Done():
		f.cancel()
		<-done
		return ctx.Err()
	}
}
