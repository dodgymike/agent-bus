package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
}

// ForwarderStats is the observable state of a Forwarder. Queued counts jobs
// accepted onto a queue; Sent counts peer answers received (including a loop
// drop, which is a settled answer); Failed counts transport or refusal errors.
type ForwarderStats struct {
	Queued  uint64
	Sent    uint64
	Dropped DropCounts
	Failed  uint64

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
	PeerBaseURL func(peerBusID string) (string, bool)

	// QueueDepth is the per-peer queue depth. 0 means DefaultQueueDepth;
	// negative is a construction error.
	QueueDepth int

	// Timeout bounds one outbound attempt. 0 means DefaultForwardTimeout;
	// negative is a construction error.
	Timeout time.Duration

	// Logger is optional; nil discards.
	Logger *logging.Logger
}

// relayJob is one outbound attempt: an already-built envelope and where it goes.
type relayJob struct {
	peerBusID string
	baseURL   string
	req       RelayRequest
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
// # THIS QUEUE IS IN-MEMORY AND THEREFORE LOSSY. DO NOT OVERCLAIM DELIVERY.
//
// Stated plainly because the honest version is easy to lose: a message accepted
// by Enqueue is NOT guaranteed to reach the peer. It is lost if the process
// crashes with the queue non-empty, and it is dropped — counted in
// Dropped.Full, logged at Warn — if the peer stays down long enough for its
// queue to fill. There is NO DURABLE RELAY OUTBOX and NO RETRY here. RELAY-4
// owns retry and backoff; a durable outbox is a follow-up that must be filed
// rather than assumed. Until both exist, cross-bus delivery is BEST EFFORT and
// nothing in the product should claim otherwise.
type Forwarder struct {
	busID       string
	reg         *Registry
	client      *Client
	peerBaseURL func(string) (string, bool)
	depth       int
	timeout     time.Duration
	log         *logging.Logger

	// ctx/cancel abort in-flight requests when Close gives up waiting, so no
	// goroutine can outlive Close even when a peer is hanging.
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	queues map[string]chan relayJob
	wg     sync.WaitGroup

	queued      atomic.Uint64
	sent        atomic.Uint64
	failed      atomic.Uint64
	dropFull    atomic.Uint64
	dropLoop    atomic.Uint64
	dropNoRoute atomic.Uint64
	workers     atomic.Int64
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
	depth := opts.QueueDepth
	if depth == 0 {
		depth = DefaultQueueDepth
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultForwardTimeout
	}
	log := opts.Logger
	if log == nil {
		log = logging.New(io.Discard, logging.LevelError)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Forwarder{
		busID:       opts.BusID,
		reg:         opts.Registry,
		client:      opts.Client,
		peerBaseURL: opts.PeerBaseURL,
		depth:       depth,
		timeout:     timeout,
		log:         log,
		ctx:         ctx,
		cancel:      cancel,
		queues:      make(map[string]chan relayJob),
	}, nil
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
		accepted, err := f.offer(relayJob{peerBusID: peerBusID, baseURL: baseURL, req: req})
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
		f.deliver(job)
	}
	f.log.Debug("relay peer worker stopped", "local_bus", f.busID, "peer_bus", peerBusID)
}

// deliver performs one outbound relay attempt.
//
// The per-attempt context descends from the Forwarder's own, so Close can abort
// an in-flight request; without that, a peer that accepts a connection and
// never answers would keep a goroutine alive past Close and the -race build
// would (rightly) report a goroutine leak.
func (f *Forwarder) deliver(job relayJob) {
	ctx, cancel := context.WithTimeout(f.ctx, f.timeout)
	defer cancel()

	resp, err := f.client.Relay(ctx, job.baseURL, job.req)
	if err != nil {
		f.failed.Add(1)
		// No retry here: RELAY-4 owns retry and backoff, and retrying without a
		// durable outbox would only re-send from memory we may lose anyway.
		f.log.Warn("relay forward failed",
			"local_bus", f.busID,
			"peer_bus", job.peerBusID,
			"origin_message_id", job.req.MessageID,
			"err", err.Error(),
		)
		return
	}
	f.sent.Add(1)
	if !resp.Accepted && resp.DroppedReason == DropLoop {
		// A settled, expected outcome in a cyclic topology, NOT a failure. It
		// is counted as a loop drop so the mesh's shape is visible, and it is
		// explicitly not retried.
		f.dropLoop.Add(1)
		f.log.Debug("peer dropped a relayed message as a loop",
			"local_bus", f.busID, "peer_bus", job.peerBusID, "origin_message_id", job.req.MessageID)
	}
}

// Stats reports the forwarder's counters.
func (f *Forwarder) Stats() ForwarderStats {
	return ForwarderStats{
		Queued:  f.queued.Load(),
		Sent:    f.sent.Load(),
		Failed:  f.failed.Load(),
		Workers: f.workers.Load(),
		Dropped: DropCounts{
			Full:    f.dropFull.Load(),
			Loop:    f.dropLoop.Load(),
			NoRoute: f.dropNoRoute.Load(),
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
