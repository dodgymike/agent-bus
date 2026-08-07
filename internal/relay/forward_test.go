package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// peerServer is a stand-in for a peer bus's relay ingress: it either answers
// immediately or HANGS until the request context is cancelled, which is how a
// dead-but-listening peer behaves.
type peerServer struct {
	srv      *httptest.Server
	received atomic.Int64
	hanging  atomic.Int64
	release  chan struct{}
}

func newPeerServer(t *testing.T, hang bool) *peerServer {
	t.Helper()
	p := &peerServer{release: make(chan struct{})}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The body is drained even though this stand-in does not parse it.
		// net/http only starts watching a connection for the client going away
		// once the request body has hit EOF, so a handler that never reads the
		// body never learns that its caller disconnected — and r.Context()
		// below would never fire.
		_, _ = io.Copy(io.Discard, r.Body)
		if hang {
			p.hanging.Add(1)
			select {
			case <-p.release:
			case <-r.Context().Done():
				// The forwarder cancelled the request (Close, or the per-attempt
				// timeout). Unwinding here is what lets the server goroutine
				// exit too, so the leak check below is meaningful.
				p.hanging.Add(-1)
				return
			}
			p.hanging.Add(-1)
		}
		p.received.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RelayResponse{Accepted: true, MessageID: "bus-far-1"})
	}))
	t.Cleanup(func() {
		close(p.release)
		p.srv.Close()
	})
	return p
}

// forwarderFixture wires a Registry, a Client and a Forwarder around a set of
// peer servers.
type forwarderFixture struct {
	reg   *Registry
	fwd   *Forwarder
	peers map[string]*peerServer
}

func newForwarderFixture(t *testing.T, depth int, peers map[string]*peerServer) *forwarderFixture {
	t.Helper()
	reg, err := NewRegistry(RegistryOptions{BusID: localBus})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	var anySrv *httptest.Server
	for busID, p := range peers {
		if err := reg.UpsertPeer(PeerRoster{BusID: busID, Agents: []string{busID + ".beta-1"}}); err != nil {
			t.Fatalf("UpsertPeer(%s): %v", busID, err)
		}
		anySrv = p.srv
	}
	cli, err := NewClient(ClientConfig{
		BusID:       localBus,
		LocalRoster: func() []string { return nil },
		HTTPClient:  anySrv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	fwd, err := NewForwarder(ForwarderOptions{
		BusID:      localBus,
		Registry:   reg,
		Client:     cli,
		QueueDepth: depth,
		Timeout:    5 * time.Second,
		PeerBaseURL: func(busID string) (string, bool) {
			p, ok := peers[busID]
			if !ok {
				return "", false
			}
			return p.srv.URL, true
		},
	})
	if err != nil {
		t.Fatalf("NewForwarder: %v", err)
	}
	return &forwarderFixture{reg: reg, fwd: fwd, peers: peers}
}

// localMessage is a message this bus originated, ready to be forwarded. Its
// path is empty, which Forward turns into exactly our own hop.
func localMessage(seq uint64, recipients []string, broadcast bool) RelayedMessage {
	// The recipient set and the broadcast flag are set BEFORE the fixture signs,
	// so what the forwarder carries is a genuinely signed envelope rather than
	// one whose signature stopped covering its own recipients.
	return originMessage(localBus, localBus+".alpha-1", seq, []byte("forward me"), func(m *RelayedMessage) {
		m.Recipients = recipients
		m.Broadcast = broadcast
		if broadcast {
			m.Recipients = nil
		}
	})
}

// TestForwarderNeverBlocksOnASlowPeer is the structural half of "a slow or dead
// peer must never make a local send slow or fail". The peer below accepts the
// connection and then never answers; Enqueue must still return essentially
// instantly, every time.
func TestForwarderNeverBlocksOnASlowPeer(t *testing.T) {
	dead := newPeerServer(t, true)
	f := newForwarderFixture(t, 4, map[string]*peerServer{"bus-dead": dead})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.fwd.Close(ctx)
	})

	start := time.Now()
	for i := uint64(1); i <= 64; i++ {
		if _, err := f.fwd.Enqueue(localMessage(i, []string{"bus-dead.beta-1"}, false)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("64 enqueues against a hung peer took %s; Enqueue must NEVER block, or a dead peer would slow every local send", elapsed)
	}

	// The queue filled and the surplus was DROPPED and COUNTED — lossy by
	// design, and observable rather than silent.
	stats := f.fwd.Stats()
	if stats.Dropped.Full == 0 {
		t.Fatalf("stats = %+v, want a non-zero Dropped.Full: the queue is bounded and the surplus must be counted", stats)
	}
	if stats.Queued > 5 { // depth 4 plus the one in flight
		t.Errorf("stats = %+v, want at most depth+1 queued against a hung peer", stats)
	}
}

// TestForwarderADeadPeerDoesNotStarveAHealthyOne is the reason for one queue
// and one goroutine PER PEER. With a shared pool, the hung peer below would
// occupy the workers and the healthy peer would never be served —
// head-of-line blocking, where the least important peer takes out the rest.
func TestForwarderADeadPeerDoesNotStarveAHealthyOne(t *testing.T) {
	dead := newPeerServer(t, true)
	live := newPeerServer(t, false)
	f := newForwarderFixture(t, 8, map[string]*peerServer{"bus-dead": dead, "bus-live": live})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.fwd.Close(ctx)
	})

	// A broadcast fans out to both peers.
	for i := uint64(1); i <= 3; i++ {
		queued, err := f.fwd.Enqueue(localMessage(i, nil, true))
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if queued != 2 {
			t.Fatalf("Enqueue queued %d copies, want 2 (one per peer)", queued)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for live.received.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := live.received.Load(); got != 3 {
		t.Fatalf("the healthy peer received %d of 3 messages while another peer hung; per-peer queues exist precisely so one dead peer cannot starve the others", got)
	}
	if got := dead.received.Load(); got != 0 {
		t.Errorf("the hung peer somehow completed %d requests", got)
	}
}

// TestForwarderCloseLeavesNoGoroutineRunning pins the shutdown contract: no
// goroutine outlives Close, even when a peer has accepted a connection and gone
// silent. Close cancels in-flight requests and STILL waits for the workers to
// unwind before returning.
func TestForwarderCloseLeavesNoGoroutineRunning(t *testing.T) {
	dead := newPeerServer(t, true)
	live := newPeerServer(t, false)
	// Let the servers' own goroutines settle before the baseline is taken.
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	f := newForwarderFixture(t, 4, map[string]*peerServer{"bus-dead": dead, "bus-live": live})
	for i := uint64(1); i <= 6; i++ {
		if _, err := f.fwd.Enqueue(localMessage(i, nil, true)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	// Wait until the hung peer is actually holding a request, so Close has
	// something to cancel rather than an already-idle worker.
	deadline := time.Now().Add(5 * time.Second)
	for dead.hanging.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if dead.hanging.Load() == 0 {
		t.Fatal("the hung peer never received a request, so this test would prove nothing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	closeStart := time.Now()
	err := f.fwd.Close(ctx)
	closeTook := time.Since(closeStart)
	if err != nil && err != context.DeadlineExceeded {
		t.Fatalf("Close: %v", err)
	}
	if closeTook > 5*time.Second {
		t.Fatalf("Close took %s; it must abandon a hung peer rather than wait for it", closeTook)
	}

	// A second Close is a no-op, and Enqueue after Close is refused as a LOCAL
	// lifecycle fault.
	if err := f.fwd.Close(context.Background()); err != nil {
		t.Errorf("a second Close returned %v, want nil", err)
	}
	if _, err := f.fwd.Enqueue(localMessage(99, nil, true)); err != ErrForwarderClosed {
		t.Errorf("Enqueue after Close returned %v, want ErrForwarderClosed", err)
	}

	// THE ASSERTION THAT MATTERS: Close waits on the WaitGroup in BOTH branches,
	// so every per-peer worker has already exited by the time it returns. The
	// Workers counter is decremented in each worker's own defer, so a non-zero
	// value here would mean a goroutine outlived Close.
	if got := f.fwd.Stats().Workers; got != 0 {
		t.Fatalf("Workers = %d after Close, want 0: no goroutine may outlive Close", got)
	}

	// And the in-flight request really was cancelled rather than merely
	// abandoned — the hung peer's handler unwound on its own request context.
	for deadline := time.Now().Add(5 * time.Second); dead.hanging.Load() != 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if got := dead.hanging.Load(); got != 0 {
		t.Fatalf("%d request(s) are still hanging at the dead peer after Close; the in-flight request was not cancelled", got)
	}

	// A backstop on the raw count, polled because the test servers' own
	// connection goroutines unwind asynchronously and the shared http.Transport
	// keeps idle connections alive.
	settled := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if runtime.NumGoroutine() <= baseline+8 {
			settled = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !settled {
		t.Fatalf("goroutines did not settle after Close: %d running, baseline %d", runtime.NumGoroutine(), baseline)
	}
}

// TestForwarderEnqueueIsSafeAgainstAConcurrentClose is the REGRESSION TEST for
// a send-on-closed-channel PANIC, and it is the reason offer() holds f.mu
// across the queue send instead of merely across the map lookup.
//
// # The bug it reproduces
//
// offer() used to take f.mu, look up the peer's channel, RELEASE the lock, and
// only then send on the channel. Close closes every queue channel while holding
// f.mu, so a Close landing inside that window closed the channel between the
// lookup and the send — and a send on a closed channel is an unrecoverable
// panic. Nothing catches it: the server dies, and it dies for the crime of
// shutting down while a message was being forwarded, which is to say it dies
// most reliably in exactly the situation where a clean shutdown mattered.
//
// A panic in a goroutine takes the whole test binary down, so "the test passed"
// IS the assertion here — there is no err to inspect. What the test has to earn
// is that the WINDOW WAS ACTUALLY OPEN, because a race test that never races is
// indistinguishable from a fixed bug. Three things do that:
//
//   - one seed Enqueue per round, so both per-peer channels (and their
//     goroutines) already exist before the storm: the send Close would have
//     raced is a send onto a channel that is really there;
//   - the Close is fired from its own goroutine at a per-round JITTER, so it
//     lands at a different point in the enqueue storm each time rather than
//     always winning or always losing;
//   - the counters below refuse to let the test pass unless BOTH outcomes were
//     observed — at least one Enqueue accepted and at least one refused with
//     ErrForwarderClosed. That is the executable statement that Close really
//     did interleave with Enqueue.
//
// It also pins the two properties the shutdown contract owes an operator:
// Stats().Workers is 0 once Close returns (no goroutine outlives it), and no
// single Enqueue ever waits on the network — one of the two peers here accepts
// the connection and then never answers, so an Enqueue that could block would
// block for the forwarder's whole 5s attempt timeout, not for the milliseconds
// the bound below allows.
func TestForwarderEnqueueIsSafeAgainstAConcurrentClose(t *testing.T) {
	dead := newPeerServer(t, true)
	live := newPeerServer(t, false)
	peers := map[string]*peerServer{"bus-dead": dead, "bus-live": live}

	const (
		rounds    = 8
		writers   = 8
		perWriter = 150
		// Far below the forwarder's 5s per-attempt timeout, so an Enqueue that
		// had somehow ended up waiting on the hung peer could not hide under it.
		enqueueBudget = 2 * time.Second
	)

	var (
		seq      uint64
		accepted int64
		refused  int64
		slowest  int64
	)

	for round := 0; round < rounds; round++ {
		f := newForwarderFixture(t, 2, peers)

		// Seed both per-peer queues and their goroutines BEFORE the race, so the
		// channel Close closes is one an Enqueue is genuinely reaching for.
		if _, err := f.fwd.Enqueue(localMessage(atomic.AddUint64(&seq, 1), nil, true)); err != nil {
			t.Fatalf("round %d: the seeding Enqueue failed: %v", round, err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		unexpected := make(chan error, writers*perWriter)

		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < perWriter; i++ {
					m := localMessage(atomic.AddUint64(&seq, 1), nil, true)
					began := time.Now()
					_, err := f.fwd.Enqueue(m)
					took := int64(time.Since(began))
					for {
						prev := atomic.LoadInt64(&slowest)
						if took <= prev || atomic.CompareAndSwapInt64(&slowest, prev, took) {
							break
						}
					}
					switch {
					case err == nil:
						atomic.AddInt64(&accepted, 1)
					case errors.Is(err, ErrForwarderClosed):
						atomic.AddInt64(&refused, 1)
					default:
						unexpected <- err
					}
				}
			}()
		}

		closed := make(chan error, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(time.Duration(round%4) * 100 * time.Microsecond)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			closed <- f.fwd.Close(ctx)
		}()

		close(start)
		wg.Wait()

		if err := <-closed; err != nil && err != context.DeadlineExceeded {
			t.Fatalf("round %d: Close: %v", round, err)
		}
		close(unexpected)
		for err := range unexpected {
			t.Fatalf("round %d: Enqueue returned %v; ErrForwarderClosed is the ONLY error it may return, because a relay failure is never a failure of the local send that produced it", round, err)
		}
		// Close waits on the WaitGroup in BOTH of its branches, so every per-peer
		// worker has already unwound by the time it returns.
		if got := f.fwd.Stats().Workers; got != 0 {
			t.Fatalf("round %d: Workers = %d after Close, want 0: no goroutine may outlive Close", round, got)
		}
	}

	if got := atomic.LoadInt64(&accepted); got == 0 {
		t.Fatalf("not one of the %d concurrent enqueues was accepted; Close won every round, so the close-versus-send window was never open and this test proves nothing", rounds*writers*perWriter)
	}
	if got := atomic.LoadInt64(&refused); got == 0 {
		t.Fatalf("not one of the %d concurrent enqueues saw ErrForwarderClosed; Close never overlapped the storm, so the close-versus-send window was never open and this test proves nothing", rounds*writers*perWriter)
	}
	if got := time.Duration(atomic.LoadInt64(&slowest)); got > enqueueBudget {
		t.Fatalf("the slowest Enqueue took %s (budget %s) while one peer hung and another was being closed; Enqueue is non-blocking BY CONSTRUCTION and a caller on a request path must never wait on a peer", got, enqueueBudget)
	}
}

// TestForwarderAppliesTheSplitHorizonAndRoutes covers the routing decisions
// Enqueue makes before anything is queued.
func TestForwarderAppliesTheSplitHorizonAndRoutes(t *testing.T) {
	live := newPeerServer(t, false)
	other := newPeerServer(t, false)
	f := newForwarderFixture(t, 8, map[string]*peerServer{"bus-live": live, "bus-other": other})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.fwd.Close(ctx)
	})

	t.Run("a DM goes only to the peer that owns the recipient", func(t *testing.T) {
		queued, err := f.fwd.Enqueue(localMessage(1, []string{"bus-live.beta-1"}, false))
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if queued != 1 {
			t.Fatalf("queued %d copies, want 1", queued)
		}
	})

	t.Run("two recipients on one peer are one outbound copy", func(t *testing.T) {
		before := f.fwd.Stats().Queued
		queued, err := f.fwd.Enqueue(localMessage(2, []string{"bus-live.beta-1", "bus-live.gamma-2"}, false))
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if queued != 1 {
			t.Fatalf("queued %d copies for two recipients on one peer, want 1", queued)
		}
		if got := f.fwd.Stats().Queued - before; got != 1 {
			t.Fatalf("Queued moved by %d, want 1", got)
		}
	})

	t.Run("an unroutable recipient is counted as no-route", func(t *testing.T) {
		before := f.fwd.Stats().Dropped.NoRoute
		queued, err := f.fwd.Enqueue(localMessage(3, []string{"bus-nobody.x-1", localBus + ".alpha-2"}, false))
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if queued != 0 {
			t.Fatalf("queued %d copies for unroutable recipients, want 0", queued)
		}
		if got := f.fwd.Stats().Dropped.NoRoute - before; got != 2 {
			t.Errorf("Dropped.NoRoute moved by %d, want 2 (an unknown bus and a LOCAL delivery)", got)
		}
	})

	t.Run("the split horizon skips a peer already on the path", func(t *testing.T) {
		m := localMessage(4, []string{"bus-live.beta-1"}, false)
		// The message has already been to bus-live; forwarding it back is the
		// cycle traffic the split horizon exists to stop.
		m.BusPath = []string{"bus-live"}
		m.OriginBus = "bus-live"
		before := f.fwd.Stats().Dropped.Loop
		queued, err := f.fwd.Enqueue(m)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if queued != 0 {
			t.Fatalf("queued %d copies back to a bus already on the path, want 0", queued)
		}
		if got := f.fwd.Stats().Dropped.Loop - before; got != 1 {
			t.Errorf("Dropped.Loop moved by %d, want 1", got)
		}
	})

	t.Run("a broadcast excludes peers already on the path", func(t *testing.T) {
		m := localMessage(5, nil, true)
		m.BusPath = []string{"bus-other"}
		m.OriginBus = "bus-other"
		queued, err := f.fwd.Enqueue(m)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if queued != 1 {
			t.Fatalf("queued %d copies, want 1 (bus-other is already on the path)", queued)
		}
	})

	t.Run("a peer with no known base URL is counted as no-route", func(t *testing.T) {
		if err := f.reg.UpsertPeer(PeerRoster{BusID: "bus-addressless", Agents: []string{"bus-addressless.x-1"}}); err != nil {
			t.Fatalf("UpsertPeer: %v", err)
		}
		before := f.fwd.Stats().Dropped.NoRoute
		queued, err := f.fwd.Enqueue(localMessage(6, []string{"bus-addressless.x-1"}, false))
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if queued != 0 {
			t.Fatalf("queued %d copies to a peer with no address, want 0", queued)
		}
		if got := f.fwd.Stats().Dropped.NoRoute - before; got != 1 {
			t.Errorf("Dropped.NoRoute moved by %d, want 1", got)
		}
	})
}

// TestForwarderDeliversAndCountsSent walks one message all the way to a real
// peer handler and back, so the counters are proved against real traffic.
func TestForwarderDeliversAndCountsSent(t *testing.T) {
	live := newPeerServer(t, false)
	f := newForwarderFixture(t, 8, map[string]*peerServer{"bus-live": live})

	if _, err := f.fwd.Enqueue(localMessage(1, []string{"bus-live.beta-1"}, false)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.fwd.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := live.received.Load(); got != 1 {
		t.Fatalf("the peer received %d messages, want 1", got)
	}
	stats := f.fwd.Stats()
	if stats.Queued != 1 || stats.Sent != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want Queued=1 Sent=1 Failed=0", stats)
	}
}

func TestNewForwarderRejectsIncompleteOptions(t *testing.T) {
	reg, err := NewRegistry(RegistryOptions{BusID: localBus})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	cli, err := NewClient(ClientConfig{
		BusID:       localBus,
		LocalRoster: func() []string { return nil },
		HTTPClient:  &http.Client{},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	base := func(string) (string, bool) { return "https://peer.example", true }

	for _, tc := range []struct {
		name string
		opts ForwarderOptions
	}{
		{"no bus id", ForwarderOptions{Registry: reg, Client: cli, PeerBaseURL: base}},
		{"no registry", ForwarderOptions{BusID: localBus, Client: cli, PeerBaseURL: base}},
		{"no client", ForwarderOptions{BusID: localBus, Registry: reg, PeerBaseURL: base}},
		{"no base URL resolver", ForwarderOptions{BusID: localBus, Registry: reg, Client: cli}},
		{"negative queue depth", ForwarderOptions{BusID: localBus, Registry: reg, Client: cli, PeerBaseURL: base, QueueDepth: -1}},
		{"negative timeout", ForwarderOptions{BusID: localBus, Registry: reg, Client: cli, PeerBaseURL: base, Timeout: -1}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewForwarder(tc.opts); err == nil {
				t.Fatal("NewForwarder accepted an incomplete config")
			}
		})
	}
}
