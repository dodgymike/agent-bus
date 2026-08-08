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
type DropCounts struct {
	// Full: the peer's queue was at its depth. LOSSY BY DESIGN — see Forwarder.
	Full uint64
	// Loop: the split horizon refused the target, or the message's own path
	// already contained us.
	Loop uint64
	// NoRoute: no peer owns the recipient, or the peer has no known base URL.
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
// # THIS QUEUE IS IN-MEMORY AND THEREFORE LOSSY. DO NOT OVERCLAIM DELIVERY.
//
// Stated plainly because the honest version is easy to lose: a message accepted
// by Enqueue is NOT guaranteed to reach the peer. It is lost if the process
// crashes with the queue non-empty, and it is dropped — counted in
// Dropped.Full, logged at Warn — if the peer stays down long enough for its
// queue to fill. RELAY-4 added RETRY; it did NOT add a DURABLE RELAY OUTBOX, and
// retry does nothing for a crash because the queue it retries from is the
// process's own memory. Until the outbox lands, cross-bus delivery is BEST
// EFFORT and nothing in the product should claim otherwise.
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
	queues map[string]chan relayJob
	wg     sync.WaitGroup

	queued        atomic.Uint64
	sent          atomic.Uint64
	failed        atomic.Uint64
	retried       atomic.Uint64
	dropFull      atomic.Uint64
	dropLoop      atomic.Uint64
	dropNoRoute   atomic.Uint64
	dropExpired   atomic.Uint64
	dropPermanent atomic.Uint64
	dropYielded   atomic.Uint64
	workers       atomic.Int64
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
	ctx, cancel := context.WithCancel(context.Background())
	return &Forwarder{
		busID:        opts.BusID,
		reg:          opts.Registry,
		client:       opts.Client,
		peerBaseURL:  opts.PeerBaseURL,
		depth:        depth,
		timeout:      timeout,
		retryHorizon: horizon,
		backoffBase:  backoffBase,
		backoffCap:   backoffCap,
		log:          log,
		ctx:          ctx,
		cancel:       cancel,
		stopping:     make(chan struct{}),
		queues:       make(map[string]chan relayJob),
		rand:         rand.New(rand.NewSource(cryptoSeed())),
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

	queued := 0
	for _, peerBusID := range targets {
		baseURL, ok := f.peerBaseURL(peerBusID)
		if !ok || baseURL == "" {
			f.dropNoRoute.Add(1)
			f.log.Warn("relay target has no known base URL",
				"local_bus", f.busID, "peer_bus", peerBusID, "origin_message_id", m.OriginMessageID)
			continue
		}
		accepted, err := f.offer(relayJob{peerBusID: peerBusID, baseURL: baseURL, req: req, enqueuedAt: f.now()})
		if err != nil {
			return queued, err
		}
		if accepted {
			f.queued.Add(1)
			queued++
		} else {
			// LOSSY, DELIBERATELY, AND COUNTED. Blocking here would be
			// back-pressure from a dead peer onto a local send.
			f.dropFull.Add(1)
			f.log.Warn("relay queue full; message dropped for this peer (the outbound queue is in-memory and has no durable outbox: RELAY-4 owns retry, and cross-bus delivery is BEST EFFORT until it lands)",
				"local_bus", f.busID,
				"peer_bus", peerBusID,
				"origin_message_id", m.OriginMessageID,
				"queue_depth", f.depth,
				"dropped_full_total", f.dropFull.Load(),
			)
		}
	}
	return queued, nil
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
			f.log.Warn("relay peer worker abandoned its queue at shutdown",
				"local_bus", f.busID, "peer_bus", peerBusID, "abandoned", len(ch)+1)
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
				f.log.Warn("relay forward abandoned at shutdown",
					"local_bus", f.busID,
					"peer_bus", job.peerBusID,
					"origin_message_id", job.req.MessageID,
					"attempts", attempt+1,
				)
				return
			}
			f.dropPermanent.Add(1)
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
			f.log.Warn("relay forward gave up: the retry horizon is exhausted and the message is LOST (there is no durable relay outbox)",
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
			f.log.Warn("relay forward abandoned mid-backoff at shutdown",
				"local_bus", f.busID, "peer_bus", job.peerBusID, "origin_message_id", job.req.MessageID)
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

// Stats reports the forwarder's counters.
func (f *Forwarder) Stats() ForwarderStats {
	return ForwarderStats{
		Queued:  f.queued.Load(),
		Sent:    f.sent.Load(),
		Failed:  f.failed.Load(),
		Retried: f.retried.Load(),
		Workers: f.workers.Load(),
		Dropped: DropCounts{
			Full:      f.dropFull.Load(),
			Loop:      f.dropLoop.Load(),
			NoRoute:   f.dropNoRoute.Load(),
			Expired:   f.dropExpired.Load(),
			Permanent: f.dropPermanent.Load(),
			Yielded:   f.dropYielded.Load(),
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
