package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
)

// RELAY-4's evidence. The task is "if a peer is unreachable, relay to it retries
// with backoff on a background path rather than blocking the local sender's
// response", and the constraint it must be designed within is not in the task
// text at all — it is in internal/idem/retention.go, which derives the
// applied-key retention window from a 24-hour PeerOutageBudget and names RELAY-4
// as the thing that must stay inside it.
//
// So this file proves four separate things, and the last one is the one nobody
// would think to look for:
//
//  1. a peer that fails and comes back is retried, and the message arrives ONCE;
//  2. the schedule is exponential with full jitter and a cap;
//  3. a refusal that can never succeed is NOT retried (a retry loop over a 400
//     would be the traffic amplifier relayhttp.go's status argument warns about);
//  4. THE TOTAL RETRY HORIZON CANNOT EXCEED idem.PeerOutageBudget, structurally,
//     because a forwarder that could out-retry the retention window turns the one
//     by-design double-apply in the system into an everyday one.

// scriptedPeer answers each request from a script indexed by attempt number, and
// records when each attempt arrived.
type scriptedPeer struct {
	srv *httptest.Server

	mu       sync.Mutex
	attempts []time.Time
	// hold, if non-nil, blocks the handler until it is closed. It models a peer
	// that has accepted the connection and is thinking.
	hold chan struct{}
}

// newScriptedPeer builds a peer whose Nth request (0-based) is answered by
// script(N). A status of 200 means "accepted"; anything else is a refusal
// carrying that status.
func newScriptedPeer(t *testing.T, script func(attempt int) int) *scriptedPeer {
	t.Helper()
	p := &scriptedPeer{}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

		p.mu.Lock()
		n := len(p.attempts)
		p.attempts = append(p.attempts, time.Now())
		hold := p.hold
		p.mu.Unlock()

		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}

		status := script(n)
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(RelayResponse{Accepted: true, MessageID: "bus-far-1"})
			return
		}
		if status >= 300 && status < 400 {
			// A redirect is surfaced as a refusal, never followed — see
			// Client.NewClient's ErrUseLastResponse policy.
			w.Header().Set("Location", "https://attacker.example/v1/peer/relay")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "scripted", "code": "scripted_refusal"})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *scriptedPeer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.attempts)
}

func (p *scriptedPeer) times() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.attempts...)
}

// retryFixture wires one scripted peer to a Forwarder whose retry knobs and test
// seams the caller controls.
type retryFixture struct {
	fwd  *Forwarder
	peer *scriptedPeer
}

func newRetryFixture(t *testing.T, peer *scriptedPeer, tune func(*ForwarderOptions), seam func(*Forwarder)) *retryFixture {
	t.Helper()
	reg, err := NewRegistry(RegistryOptions{BusID: localBus})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := reg.UpsertPeer(PeerRoster{BusID: "bus-far", Agents: []string{"bus-far.beta-1"}}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
	cli, err := NewClient(ClientConfig{
		BusID:       localBus,
		LocalRoster: func() []string { return nil },
		HTTPClient:  peer.srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	opts := ForwarderOptions{
		BusID:       localBus,
		Registry:    reg,
		Client:      cli,
		QueueDepth:  8,
		Timeout:     5 * time.Second,
		PeerBaseURL: func(string) (string, bool) { return peer.srv.URL, true },
	}
	if tune != nil {
		tune(&opts)
	}
	fwd, err := NewForwarder(opts)
	if err != nil {
		t.Fatalf("NewForwarder: %v", err)
	}
	if seam != nil {
		seam(fwd)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = fwd.Close(ctx)
	})
	return &retryFixture{fwd: fwd, peer: peer}
}

func (f *retryFixture) send(t *testing.T, seq uint64) {
	t.Helper()
	if _, err := f.fwd.Enqueue(localMessage(seq, []string{"bus-far.beta-1"}, false)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
}

// waitFor spins until cond holds, and FAILS rather than proceeding if it never
// does. Every retry assertion below needs it: Close abandons an in-flight
// backoff by design, so closing before the schedule has played out would make
// the test observe one attempt and pass a "no retry happened" reading as if it
// were the code's behaviour.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// closeNow drains the forwarder and returns its final stats.
func (f *retryFixture) closeNow(t *testing.T) ForwarderStats {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.fwd.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return f.fwd.Stats()
}

// ---------------------------------------------------------------------------
// 1. A peer that comes back gets the message, exactly once
// ---------------------------------------------------------------------------

// TestPeerRetryBackoffDeliversAfterAPeerComesBack is RELAY-4's headline case: a
// peer that is down when the local send happens, and up again shortly after.
//
// Before this task, the single attempt failed and the message was gone. The
// assertion that matters is not merely "it arrived" but "it arrived ONCE": a
// retry loop that re-sent after a successful attempt would produce the duplicate
// invariant 10 exists to suppress, and would do it from our side of the wire.
func TestPeerRetryBackoffDeliversAfterAPeerComesBack(t *testing.T) {
	peer := newScriptedPeer(t, func(attempt int) int {
		if attempt < 3 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	f := newRetryFixture(t, peer,
		func(o *ForwarderOptions) {
			o.RetryBackoffBase = MinRetryBackoffBase
			o.RetryBackoffCap = 4 * MinRetryBackoffBase
			o.RetryHorizon = 30 * time.Second
		}, nil)

	f.send(t, 1)
	waitFor(t, "the peer to see four attempts", func() bool { return peer.count() >= 4 })
	waitFor(t, "the forwarder to record the acceptance", func() bool { return f.fwd.Stats().Sent == 1 })
	stats := f.closeNow(t)

	if got := peer.count(); got != 4 {
		t.Fatalf("the peer saw %d attempts, want 4 (three refusals then the acceptance)", got)
	}
	if stats.Sent != 1 {
		t.Errorf("Sent = %d, want 1: exactly one attempt was ACCEPTED, and a retry after acceptance would be a duplicate we manufactured ourselves", stats.Sent)
	}
	if stats.Retried != 3 {
		t.Errorf("Retried = %d, want 3", stats.Retried)
	}
	if stats.Failed != 3 {
		t.Errorf("Failed = %d, want 3 (the three 503s)", stats.Failed)
	}
	if stats.Dropped != (DropCounts{}) {
		t.Errorf("Dropped = %+v, want all zero: the message was delivered", stats.Dropped)
	}
}

// ---------------------------------------------------------------------------
// 2. The schedule: exponential, capped, full jitter
// ---------------------------------------------------------------------------

// TestPeerRetryBackoffScheduleDoublesAndCaps asserts the WINDOW sequence
// exactly, by intercepting the jitter draw. Asserting the sequence rather than
// timing the sleeps is deliberate: a wall-clock assertion on a backoff schedule
// is the classic flaky test, and it would also be satisfied by a schedule that
// merely happened to be slow.
func TestPeerRetryBackoffScheduleDoublesAndCaps(t *testing.T) {
	peer := newScriptedPeer(t, func(int) int { return http.StatusServiceUnavailable })

	var mu sync.Mutex
	var windows []time.Duration
	f := newRetryFixture(t, peer,
		func(o *ForwarderOptions) {
			o.RetryBackoffBase = 10 * time.Millisecond
			o.RetryBackoffCap = 80 * time.Millisecond
			o.RetryHorizon = 30 * time.Second
		},
		func(fwd *Forwarder) {
			fwd.jitterFn = func(w time.Duration) time.Duration {
				mu.Lock()
				windows = append(windows, w)
				n := len(windows)
				mu.Unlock()
				if n >= 7 {
					// Enough schedule observed; stop the run rather than
					// retrying for the full horizon.
					go func() {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						_ = fwd.Close(ctx)
					}()
				}
				return 0 // no real sleeping; the WINDOW is what is under test
			}
		})

	f.send(t, 1)

	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := len(windows)
		mu.Unlock()
		if n >= 7 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = f.fwd.Close(ctx)

	mu.Lock()
	got := append([]time.Duration(nil), windows...)
	mu.Unlock()
	if len(got) < 7 {
		t.Fatalf("observed only %d backoff windows (%v), want at least 7", len(got), got)
	}
	want := []time.Duration{
		10 * time.Millisecond, // base
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond, // reaches the cap
		80 * time.Millisecond, // and stays there
		80 * time.Millisecond,
		80 * time.Millisecond,
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("backoff windows = %v, want %v: the schedule must double from the base and then hold at the cap — an uncapped schedule leaves a returning peer unprobed for an unbounded time, and a flat one hammers a peer that is deliberately shedding load", got[:len(want)], want)
		}
	}
}

// newBareForwarder builds a Forwarder with no peers, for the tests that
// exercise the schedule arithmetic directly. It goes through NewForwarder rather
// than a struct literal ON PURPOSE: a literal leaves f.rand nil, so a test that
// built one would panic the moment jitter drew — and, worse, a test that avoided
// jitter would be asserting over a Forwarder no caller can ever obtain.
func newBareForwarder(t *testing.T, tune func(*ForwarderOptions)) *Forwarder {
	t.Helper()
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
	o := ForwarderOptions{
		BusID: localBus, Registry: reg, Client: cli,
		PeerBaseURL: func(string) (string, bool) { return "https://peer.example", true },
	}
	if tune != nil {
		tune(&o)
	}
	f, err := NewForwarder(o)
	if err != nil {
		t.Fatalf("NewForwarder: %v", err)
	}
	return f
}

// envJitterProbe makes this binary print a Forwarder's jitter draws and exit,
// instead of running tests. It is how the parent below observes TWO PROCESSES.
const envJitterProbe = "RELAY_JITTER_PROBE"

// TestPeerRetryBackoffJitterProbeChild prints one fresh Forwarder's jitter
// draws. It does nothing in an ordinary run.
func TestPeerRetryBackoffJitterProbeChild(t *testing.T) {
	if os.Getenv(envJitterProbe) == "" {
		t.Skip("not a jitter probe: " + envJitterProbe + " is unset")
	}
	f := newBareForwarder(t, nil)
	var out []string
	for i := 0; i < 8; i++ {
		out = append(out, strconv.FormatInt(int64(f.jitter(time.Hour)), 10))
	}
	fmt.Println("JITTER " + strings.Join(out, ","))
}

// TestPeerRetryBackoffJitterIsNotTheFixedSeedGlobalSource is the finding both
// gates raised, turned into a test — and it has to span PROCESSES to be worth
// anything.
//
// go.mod pins `go 1.19`, where the GLOBAL math/rand source is seeded with 1
// unless GODEBUG=randautoseed=1, so `rand.Int63n` yields the SAME SEQUENCE IN
// EVERY PROCESS. That defeats the only correlation that matters here: with one
// serial worker per peer there is little to decorrelate INSIDE a process, while
// a federation recovering from a shared outage — or a rolling restart — is
// precisely a set of separate processes that must not all probe the same peer
// at the same instants.
//
// AN IN-PROCESS COMPARISON CANNOT DETECT THIS, and the first version of this
// test made exactly that mistake: two Forwarders sharing ONE global stream
// interleave their draws and so look different, so the test passed with the
// defect present. It was only caught by mutating the code back and watching the
// test stay green. Two child processes is the smallest thing that actually
// fails.
func TestPeerRetryBackoffJitterIsNotTheFixedSeedGlobalSource(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v; refusing to fall back to os.Args[0], which exec.Command would resolve through PATH", err)
	}
	probe := func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, self, "-test.run=^TestPeerRetryBackoffJitterProbeChild$", "-test.v")
		cmd.Env = append(os.Environ(), envJitterProbe+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("jitter probe child failed: %v\n%s", err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "JITTER ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "JITTER "))
			}
		}
		t.Fatalf("jitter probe child printed no draws:\n%s", out)
		return ""
	}

	a, b := probe(), probe()
	if a == "" {
		t.Fatal("the probe returned nothing; the test would be vacuous")
	}
	if a == b {
		t.Fatalf("two SEPARATE PROCESSES drew the identical jitter sequence:\n  %s\n"+
			"That is what the global math/rand does under go1.19 (fixed seed 1). It means every bus in a "+
			"federation coming back from a shared outage — or a rolling restart — probes the same peer at the "+
			"same instants, which is the thundering herd full jitter exists to prevent. Each Forwarder must own "+
			"a source seeded from crypto/rand.", a)
	}
}

// TestPeerRetryBackoffYieldsAFullQueueHead is the poison-message case: a peer
// that is perfectly HEALTHY, and one envelope it refuses every time.
//
// relayhttp.go maps every unclassified AcceptRelay failure to 503, and 503 is
// retriable, so without a yield ONE bad envelope would hold that peer's only
// worker for the whole 24-hour horizon while every other message queued behind
// it was dropped. The "a dead peer only damages its own queue" argument does not
// cover this: the peer is answering everything else fine.
func TestPeerRetryBackoffYieldsAFullQueueHead(t *testing.T) {
	hold := make(chan struct{})
	peer := newScriptedPeer(t, func(int) int { return http.StatusServiceUnavailable })
	peer.mu.Lock()
	peer.hold = hold
	peer.mu.Unlock()

	f := newRetryFixture(t, peer, func(o *ForwarderOptions) {
		o.QueueDepth = 2
		o.RetryHorizon = 30 * time.Second
		o.RetryBackoffBase = 10 * time.Millisecond
		o.RetryBackoffCap = 20 * time.Millisecond
		o.Timeout = 5 * time.Second
	}, nil)

	// Fill the queue behind the head while the head is still in flight.
	f.send(t, 1)
	waitFor(t, "the head job to reach the peer", func() bool { return peer.count() >= 1 })
	f.send(t, 2)
	f.send(t, 3)
	waitFor(t, "the queue to be full", func() bool { return f.fwd.Stats().Queued == 3 })
	close(hold)

	waitFor(t, "the head to yield", func() bool { return f.fwd.Stats().Dropped.Yielded >= 1 })
	stats := f.closeNow(t)
	if stats.Dropped.Yielded == 0 {
		t.Fatalf("stats = %+v; a retriable failure on a FULL queue must yield the head, or one poison envelope holds a healthy peer's only worker for the whole retry horizon while everything behind it is dropped", stats)
	}
}

// TestPeerRetryBackoffAbandonsAJobWhosePeerWasDePeered is the revocation
// property. The address is re-resolved on EVERY attempt, never frozen at
// enqueue.
//
// Retry stretched the gap between the routing decision and the last POST from
// one attempt to the whole horizon. With a frozen address, de-peering a
// compromised bus — or moving a peer off a hijacked address — would not take
// effect until the next day: the queued job would keep posting the sender,
// recipients and body at the old address for the rest of the horizon.
func TestPeerRetryBackoffAbandonsAJobWhosePeerWasDePeered(t *testing.T) {
	peer := newScriptedPeer(t, func(int) int { return http.StatusServiceUnavailable })

	var mu sync.Mutex
	known := true
	f := newRetryFixture(t, peer, func(o *ForwarderOptions) {
		o.RetryHorizon = 30 * time.Second
		o.RetryBackoffBase = 10 * time.Millisecond
		o.RetryBackoffCap = 20 * time.Millisecond
		o.PeerBaseURL = func(string) (string, bool) {
			mu.Lock()
			defer mu.Unlock()
			if !known {
				return "", false
			}
			return peer.srv.URL, true
		}
	}, nil)

	f.send(t, 1)
	waitFor(t, "the job to be retrying", func() bool { return f.fwd.Stats().Retried >= 1 })

	// The operator de-peers the bus mid-retry.
	mu.Lock()
	known = false
	mu.Unlock()

	waitFor(t, "the job to be abandoned", func() bool { return f.fwd.Stats().Dropped.NoRoute >= 1 })
	before := peer.count()
	stats := f.closeNow(t)
	if stats.Dropped.NoRoute == 0 {
		t.Fatalf("stats = %+v; a de-peered bus must abandon its queued jobs on the NEXT attempt, not keep posting at an address nobody vouches for any more", stats)
	}
	if got := peer.count(); got > before+1 {
		t.Errorf("the peer received %d attempts after de-peering (was %d); the address must be re-resolved per attempt", got, before)
	}
}

// TestPeerRetryBackoffUsesFullJitterNotTheWholeWindow pins the jitter POLICY.
//
// Full jitter — a uniform draw from [0, window) — rather than sleeping the whole
// window, because several messages queued for one peer would otherwise retry in
// a synchronised burst the moment it came back, turning its recovery into its
// next outage. A schedule that always slept the full window would pass every
// other test in this file.
func TestPeerRetryBackoffUsesFullJitterNotTheWholeWindow(t *testing.T) {
	f := newBareForwarder(t, func(o *ForwarderOptions) {
		o.RetryBackoffBase = time.Second
		o.RetryBackoffCap = 4 * time.Second
	})

	const draws = 512
	window := time.Second
	seen := make(map[time.Duration]struct{}, draws)
	for i := 0; i < draws; i++ {
		d := f.jitter(window)
		if d < 0 || d >= window {
			t.Fatalf("jitter drew %s, which is outside [0, %s)", d, window)
		}
		seen[d] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("%d draws produced %d distinct value(s); the sleep is not jittered at all, so every message queued for a recovering peer retries in the same instant", draws, len(seen))
	}

	// And the window itself still doubles and caps.
	for attempt, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		if got := f.backoffWindow(attempt); got != want {
			t.Errorf("backoffWindow(%d) = %s, want %s", attempt, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. "Never" is not retried
// ---------------------------------------------------------------------------

// TestPeerRetryBackoffDoesNotRetryAPermanentRefusal is the arm that keeps the
// control from becoming the amplifier.
//
// relayhttp.go's status argument turns on exactly this: a loop drop is a 200
// BECAUSE a 5xx would have RELAY-4 re-deliver forever a message that can never
// be accepted. The mirror obligation lives here — RELAY-4 must actually not
// retry the statuses that mean "never". A 400 malformed envelope, a 403 bad
// signature or unpeered bus, a 409 idempotency violation and a 413 are verdicts
// on the message's CONTENT, and the content does not change on a second try.
func TestPeerRetryBackoffDoesNotRetryAPermanentRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"400 malformed envelope", http.StatusBadRequest},
		{"403 bad signature or unpeered bus", http.StatusForbidden},
		{"409 idempotency violation", http.StatusConflict},
		{"413 too large", http.StatusRequestEntityTooLarge},
		{"307 redirect, which is a refusal and never an instruction", http.StatusTemporaryRedirect},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			peer := newScriptedPeer(t, func(int) int { return tc.status })
			f := newRetryFixture(t, peer, func(o *ForwarderOptions) {
				o.RetryBackoffBase = MinRetryBackoffBase
				o.RetryBackoffCap = 2 * MinRetryBackoffBase
				o.RetryHorizon = 30 * time.Second
			}, nil)

			f.send(t, 1)
			waitFor(t, "the permanent refusal to be recorded", func() bool { return f.fwd.Stats().Dropped.Permanent == 1 })
			stats := f.closeNow(t)

			if got := peer.count(); got != 1 {
				t.Errorf("the peer saw %d attempts for a %d, want exactly 1: resending identical bytes cannot change this answer, and retrying it makes the retry loop the traffic amplifier relayhttp.go's status argument exists to prevent", got, tc.status)
			}
			if stats.Retried != 0 {
				t.Errorf("Retried = %d, want 0", stats.Retried)
			}
			if stats.Dropped.Permanent != 1 {
				t.Errorf("Dropped.Permanent = %d, want 1 — an operator must be able to tell 'fix the message or the peering' from 'fix the link'", stats.Dropped.Permanent)
			}
			if stats.Dropped.Expired != 0 {
				t.Errorf("Dropped.Expired = %d, want 0: nothing here ran out of horizon", stats.Dropped.Expired)
			}
		})
	}
}

// TestPeerRetryBackoffRetriabilityIsDecidedByStatus pins the classification
// directly, including the boundaries, so the table above cannot pass by
// coincidence.
func TestPeerRetryBackoffRetriabilityIsDecidedByStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusMovedPermanently, false},
		{http.StatusTemporaryRedirect, false},
		{http.StatusBadRequest, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusRequestTimeout, true},
		{http.StatusConflict, false},
		{http.StatusRequestEntityTooLarge, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
	} {
		err := &PeerRefusedError{Endpoint: "https://peer.example", StatusCode: tc.status, Code: "x"}
		if got := err.Retriable(); got != tc.want {
			t.Errorf("Retriable(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. THE CONSTRAINT: the retry horizon lives inside idem.PeerOutageBudget
// ---------------------------------------------------------------------------

// TestPeerRetryBackoffHorizonStaysInsideTheOutageBudget is the assertion this
// task exists to make un-skippable.
//
// internal/idem/retention.go derives the applied-key retention window as
// 2 x (PeerOutageBudget + SessionLifetimeMax + ParkedPollMax +
// TransportRetryHorizon) = 50h10m22s, and says of the first term: "RELAY-4 ... is
// NOT yet implemented, so this is not read off an existing ceiling — it is the
// BUDGET RELAY-4 must design within ... if RELAY-4's total retry horizon ever
// exceeds this, a returning peer's retry falls outside the window and is applied
// as a new operation."
//
// That last sentence is the whole point. A retry that arrives after the
// receiving bus has forgotten the applied key is not suppressed — it is applied
// as a NEW operation and DELIVERED A SECOND TIME. So a forwarder configured with
// too long a horizon would not fail, or log, or degrade: it would silently
// double-deliver, hours later, and only under an outage long enough that nobody
// is watching. That is why the bound is enforced at CONSTRUCTION and asserted
// here, rather than left as a comment in a file nobody re-reads.
func TestPeerRetryBackoffHorizonStaysInsideTheOutageBudget(t *testing.T) {
	t.Run("the ceiling IS the peer outage budget, by reference", func(t *testing.T) {
		if RetryHorizonCeiling != idem.PeerOutageBudget {
			t.Fatalf("RetryHorizonCeiling = %s, want idem.PeerOutageBudget (%s): the two must be the same constant, or a change to the retention derivation leaves this forwarder retrying past a window that has moved", RetryHorizonCeiling, idem.PeerOutageBudget)
		}
		if idem.PeerOutageBudget != 24*time.Hour {
			t.Fatalf("idem.PeerOutageBudget = %s, want 24h; if the budget moved deliberately, the retention derivation and this bound both need re-checking", idem.PeerOutageBudget)
		}
	})

	t.Run("the default horizon plus a full attempt fits inside it", func(t *testing.T) {
		if got := DefaultRetryHorizon + DefaultForwardTimeout; got > RetryHorizonCeiling {
			t.Fatalf("DefaultRetryHorizon (%s) + DefaultForwardTimeout (%s) = %s, which EXCEEDS the %s ceiling. The last attempt may START at the deadline and then run for a full timeout, so this sum — not the horizon alone — is what has to fit",
				DefaultRetryHorizon, DefaultForwardTimeout, got, RetryHorizonCeiling)
		}
		if DefaultRetryHorizon != RetryHorizonCeiling-DefaultForwardTimeout {
			t.Fatalf("DefaultRetryHorizon = %s, want %s: it is DERIVED (the ceiling minus one full attempt), not chosen", DefaultRetryHorizon, RetryHorizonCeiling-DefaultForwardTimeout)
		}
		if want := 23*time.Hour + 59*time.Minute + 30*time.Second; DefaultRetryHorizon != want {
			t.Fatalf("DefaultRetryHorizon = %s, want %s", DefaultRetryHorizon, want)
		}
	})

	t.Run("NewForwarder REFUSES options that could out-retry the window", func(t *testing.T) {
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
		build := func(horizon, timeout time.Duration) error {
			_, err := NewForwarder(ForwarderOptions{
				BusID:        localBus,
				Registry:     reg,
				Client:       cli,
				PeerBaseURL:  func(string) (string, bool) { return "https://peer.example", true },
				RetryHorizon: horizon,
				Timeout:      timeout,
			})
			return err
		}

		if err := build(RetryHorizonCeiling-time.Second, time.Second); err != nil {
			t.Fatalf("a forwarder EXACTLY at the ceiling was refused: %v; the bound is inclusive", err)
		}
		if err := build(RetryHorizonCeiling-time.Second, time.Second+time.Nanosecond); err == nil {
			t.Fatal("a forwarder ONE NANOSECOND over the ceiling was accepted. A horizon past idem.PeerOutageBudget silently converts the derived retention window into a duplicate-delivery path: the receiving bus forgets the applied key, and the retry is applied as a NEW operation (invariant 10)")
		}
		if err := build(RetryHorizonCeiling*2, 0); err == nil {
			t.Fatal("a forwarder with double the budget as its horizon was accepted")
		}
		if err := build(-time.Second, 0); err == nil {
			t.Fatal("a negative RetryHorizon was accepted")
		}
	})

	t.Run("no attempt is issued after the horizon, in practice", func(t *testing.T) {
		peer := newScriptedPeer(t, func(int) int { return http.StatusServiceUnavailable })
		const horizon = 150 * time.Millisecond
		f := newRetryFixture(t, peer, func(o *ForwarderOptions) {
			o.RetryHorizon = horizon
			o.RetryBackoffBase = MinRetryBackoffBase
			o.RetryBackoffCap = 2 * MinRetryBackoffBase
			o.Timeout = time.Second
		}, nil)

		start := time.Now()
		f.send(t, 1)

		// Wait comfortably past the horizon, then assert nothing was attempted
		// after it and the job was accounted for as expired.
		waitFor(t, "the job to run out of horizon", func() bool { return f.fwd.Stats().Dropped.Expired == 1 })
		stats := f.closeNow(t)

		if stats.Dropped.Expired != 1 {
			t.Fatalf("Dropped.Expired = %d, want 1: a job whose horizon runs out must be counted as expired, because that counter is how an operator SEES a peer that has been down longer than the outage budget", stats.Dropped.Expired)
		}
		for i, at := range peer.times() {
			if elapsed := at.Sub(start); elapsed > horizon+time.Second {
				t.Fatalf("attempt %d landed %s after the send, past the %s horizon (plus one attempt timeout). A retry beyond the horizon is exactly the retry the receiving bus may no longer recognise", i, elapsed, horizon)
			}
		}
		if peer.count() < 2 {
			t.Fatalf("the peer saw %d attempts; the horizon test is vacuous unless at least one RETRY happened inside it", peer.count())
		}
	})
}

// TestPeerRetryBackoffDropsAJobThatExpiredWhileQueued proves the deadline is
// anchored on the ENQUEUE instant rather than on the first attempt.
//
// This is the case that makes the ceiling hold at all. Per-peer queues drain
// serially, so with a first-attempt anchor a job waiting behind a dead peer's
// head-of-line job would begin its own full horizon only after that job's had
// elapsed — two 24-hour horizons back to back against a 24-hour budget. Anchored
// on enqueue, a job that waited out its own horizon is dropped without being
// attempted, which is both correct and the honest outcome to report.
func TestPeerRetryBackoffDropsAJobThatExpiredWhileQueued(t *testing.T) {
	peer := newScriptedPeer(t, func(int) int { return http.StatusOK })
	hold := make(chan struct{})
	peer.mu.Lock()
	peer.hold = hold
	peer.mu.Unlock()

	f := newRetryFixture(t, peer, func(o *ForwarderOptions) {
		o.RetryHorizon = 80 * time.Millisecond
		o.RetryBackoffBase = MinRetryBackoffBase
		o.RetryBackoffCap = 2 * MinRetryBackoffBase
		o.Timeout = 5 * time.Second
	}, nil)

	// Two messages. The first occupies the peer's only worker while the peer
	// thinks; the second sits in the queue and ages past its own deadline.
	f.send(t, 1)
	f.send(t, 2)

	time.Sleep(300 * time.Millisecond) // > 80ms horizon, comfortably
	close(hold)

	waitFor(t, "the head job to be sent and the queued job to expire", func() bool {
		st := f.fwd.Stats()
		return st.Sent == 1 && st.Dropped.Expired == 1
	})
	stats := f.closeNow(t)

	if stats.Queued != 2 {
		t.Fatalf("Queued = %d, want 2", stats.Queued)
	}
	if stats.Sent != 1 {
		t.Errorf("Sent = %d, want 1: only the head-of-line job was still inside its horizon", stats.Sent)
	}
	if stats.Dropped.Expired != 1 {
		t.Errorf("Dropped.Expired = %d, want 1: the queued job aged past its deadline and must be dropped at dequeue, not handed a fresh horizon of its own", stats.Dropped.Expired)
	}
	if got := peer.count(); got != 1 {
		t.Errorf("the peer saw %d attempts, want 1: the expired job must never reach the wire", got)
	}
}

// ---------------------------------------------------------------------------
// The rule retry must not break: a dead peer never slows a local send
// ---------------------------------------------------------------------------

// TestPeerRetryBackoffNeverBlocksTheLocalSend re-proves RELAY-4's own headline
// sentence AFTER retry exists, because retry is the change most likely to break
// it. A retrying worker holds its peer's queue head; Enqueue must still return
// immediately, and must still never report a peer condition as an error.
func TestPeerRetryBackoffNeverBlocksTheLocalSend(t *testing.T) {
	peer := newScriptedPeer(t, func(int) int { return http.StatusServiceUnavailable })
	f := newRetryFixture(t, peer, func(o *ForwarderOptions) {
		o.RetryHorizon = 30 * time.Second
		o.RetryBackoffBase = 250 * time.Millisecond
		o.RetryBackoffCap = time.Second
		o.QueueDepth = 4
	}, nil)

	// Let the worker get well into a backoff.
	f.send(t, 1)
	time.Sleep(50 * time.Millisecond)

	for i := uint64(2); i <= 12; i++ {
		start := time.Now()
		n, err := f.fwd.Enqueue(localMessage(i, []string{"bus-far.beta-1"}, false))
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("Enqueue(%d) returned %v; the only error Enqueue may return is ErrForwarderClosed, and a peer being down is never the local sender's failure", i, err)
		}
		if elapsed > 100*time.Millisecond {
			t.Fatalf("Enqueue(%d) took %s while the peer's worker was mid-backoff; a retrying peer must never apply back-pressure to a local send (n=%d)", i, elapsed, n)
		}
	}
	// A full queue is a counted drop, never an error and never a block.
	if got := f.fwd.Stats().Dropped.Full; got == 0 {
		t.Log("note: the queue did not fill; the latency assertion above is still the load-bearing one")
	}
}

// TestPeerRetryBackoffCloseDoesNotWaitOutABackoff proves shutdown is not held
// hostage by a dead peer's schedule. Without the stopping channel, Close would
// block for up to a full backoff cap per peer that happened to be sleeping —
// making every shutdown as slow as the worst peer on the mesh.
func TestPeerRetryBackoffCloseDoesNotWaitOutABackoff(t *testing.T) {
	peer := newScriptedPeer(t, func(int) int { return http.StatusServiceUnavailable })
	f := newRetryFixture(t, peer, func(o *ForwarderOptions) {
		o.RetryHorizon = 30 * time.Second
		o.RetryBackoffBase = 20 * time.Second
		o.RetryBackoffCap = 20 * time.Second
	}, nil)

	f.send(t, 1)
	// Wait until the first attempt has been made and the worker is asleep.
	deadline := time.Now().Add(5 * time.Second)
	for peer.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if peer.count() == 0 {
		t.Fatal("the peer never saw an attempt; the test would be vacuous")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := f.fwd.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %s with a worker parked in a 20s backoff; shutdown must abandon the backoff, not wait it out", elapsed)
	}
	if got := f.fwd.Stats().Workers; got != 0 {
		t.Fatalf("Workers = %d after Close, want 0", got)
	}
}

// TestPeerRetryBackoffOptionValidation covers the remaining construction rules,
// which exist so a misconfiguration fails at startup rather than as a schedule
// that quietly goes backwards.
func TestPeerRetryBackoffOptionValidation(t *testing.T) {
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
	build := func(tune func(*ForwarderOptions)) error {
		o := ForwarderOptions{
			BusID:       localBus,
			Registry:    reg,
			Client:      cli,
			PeerBaseURL: func(string) (string, bool) { return "https://peer.example", true },
		}
		tune(&o)
		_, err := NewForwarder(o)
		return err
	}

	for _, tc := range []struct {
		name string
		tune func(*ForwarderOptions)
	}{
		{"negative backoff base", func(o *ForwarderOptions) { o.RetryBackoffBase = -time.Second }},
		{"negative backoff cap", func(o *ForwarderOptions) { o.RetryBackoffCap = -time.Second }},
		{"cap below base", func(o *ForwarderOptions) {
			o.RetryBackoffBase = 10 * time.Second
			o.RetryBackoffCap = time.Second
		}},
		{"base below the floor, which makes retry a load generator", func(o *ForwarderOptions) {
			o.RetryBackoffBase = time.Millisecond
			o.RetryBackoffCap = time.Millisecond
		}},
		{"cap above the ceiling, where the doubling would overflow to a negative duration", func(o *ForwarderOptions) {
			o.RetryBackoffBase = time.Second
			o.RetryBackoffCap = RetryHorizonCeiling + time.Second
		}},
	} {
		if err := build(tc.tune); err == nil {
			t.Errorf("%s was accepted, want a construction error", tc.name)
		}
	}

	// The defaults must construct, or every caller is forced to tune.
	if err := build(func(*ForwarderOptions) {}); err != nil {
		t.Errorf("the zero-valued retry options were refused: %v", err)
	}
}
