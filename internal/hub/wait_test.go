package hub_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// Long-poll helpers
// ---------------------------------------------------------------------------

// waitForWaiters blocks until the hub reports exactly n parked waiters, or
// fails. It is the ONLY way this file establishes "the waiter is parked": a
// bare sleep would either be flaky or slow, and would silently degrade into a
// test that never parked anything at all.
func waitForWaiters(t *testing.T, h *hub.Hub, n int, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := h.WaiterCount(); got == n {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d parked waiters (%s); WaiterCount() = %d", n, why, got)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// probeLog wraps the real durable log so a test can observe the write path.
// onWrite runs INSIDE Write, after the entry is committed and fsynced and
// before Write returns — which is exactly the window POLL-2's ordering claim
// is about.
type probeLog struct {
	inner   *wal.Log
	onWrite func()
}

func (p *probeLog) Write(e wal.Entry) (wal.Committed, error) {
	c, err := p.inner.Write(e)
	if p.onWrite != nil {
		p.onWrite()
	}
	return c, err
}

// ---------------------------------------------------------------------------
// POLL-1 — Wait's three exits: a batch, a timeout, a cancelled context
// ---------------------------------------------------------------------------

func TestLongPollWait(t *testing.T) {
	t.Run("FastPathReturnsImmediately", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		res, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("already here"), IdempotencyKey: "k-fast"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}

		const timeout = 10 * time.Second
		start := time.Now()
		batch, err := h.Wait(context.Background(), b, 0, 10, timeout)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if len(batch.Messages) != 1 || batch.Messages[0].ID != res.MessageID {
			t.Fatalf("Wait returned %d messages (%v), want just %s", len(batch.Messages), batch.Messages, res.MessageID)
		}
		if batch.TimedOut {
			t.Fatal("the fast path reported TimedOut")
		}
		if batch.Cursor != res.Seq {
			t.Fatalf("Wait returned cursor %d, want %d", batch.Cursor, res.Seq)
		}
		if elapsed > timeout/10 {
			t.Fatalf("the fast path took %v against a %v timeout; it parked when it had something to return", elapsed, timeout)
		}
		if h.WaiterCount() != 0 {
			t.Fatalf("the fast path left %d waiters registered", h.WaiterCount())
		}
	})

	t.Run("TimeoutIsNotAnError", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		b := agentID(t, h.BusID(), "beta")

		const after = uint64(0)
		const timeout = 60 * time.Millisecond

		start := time.Now()
		batch, err := h.Wait(context.Background(), b, after, 10, timeout)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("a long poll that timed out returned err = %v; a quiet bus is the normal steady state, not a failure", err)
		}
		if !batch.TimedOut {
			t.Fatal("Batch.TimedOut is false after the deadline elapsed")
		}
		if len(batch.Messages) != 0 {
			t.Fatalf("a timed-out poll returned %d messages", len(batch.Messages))
		}
		if batch.More {
			t.Fatal("a timed-out poll reported More")
		}
		if batch.Cursor != after {
			t.Fatalf("a timed-out poll returned cursor %d, want the cursor it was given (%d)", batch.Cursor, after)
		}
		if elapsed < timeout {
			t.Fatalf("Wait returned after %v, before its %v deadline", elapsed, timeout)
		}
		if h.WaiterCount() != 0 {
			t.Fatalf("a timed-out poll left %d waiters registered", h.WaiterCount())
		}
	})

	t.Run("CancelledContextBeforeParking", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		b := agentID(t, h.BusID(), "beta")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan struct{})
		var batch hub.Batch
		var err error
		go func() {
			defer close(done)
			batch, err = h.Wait(ctx, b, 0, 10, 30*time.Second)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Wait hung on an already-cancelled context")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait returned err = %v, want context.Canceled", err)
		}
		if len(batch.Messages) != 0 {
			t.Fatalf("a cancelled Wait returned %d messages", len(batch.Messages))
		}
		if h.WaiterCount() != 0 {
			t.Fatalf("a cancelled Wait left %d waiters registered", h.WaiterCount())
		}
	})

	t.Run("CancelledContextWhileParked", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		b := agentID(t, h.BusID(), "beta")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		type outcome struct {
			batch hub.Batch
			err   error
		}
		out := make(chan outcome, 1)
		go func() {
			batch, err := h.Wait(ctx, b, 0, 10, 30*time.Second)
			out <- outcome{batch, err}
		}()

		waitForWaiters(t, h, 1, "the poll must park before the context is cancelled")
		cancel()

		select {
		case got := <-out:
			if !errors.Is(got.err, context.Canceled) {
				t.Fatalf("Wait returned err = %v, want context.Canceled", got.err)
			}
			if got.batch.TimedOut {
				t.Fatal("a cancelled Wait reported TimedOut; the two exits must stay distinguishable")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Wait did not return after its context was cancelled")
		}
		waitForWaiters(t, h, 0, "a cancelled poll must deregister")
	})

	t.Run("LimitIsClamped", func(t *testing.T) {
		// Asserted through observable batch sizes only. Enough visible messages
		// to be cut by the ceiling, so both ends of clampLimit are exercised.
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		total := hub.MaxBatchLimit + 1
		for i := 0; i < total; i++ {
			if _, err := mintedBroadcast(t, h, hub.BroadcastRequest{
				Sender:         a,
				Body:           []byte(fmt.Sprintf("clamp-%d", i)),
				IdempotencyKey: fmt.Sprintf("k-waitclamp-%d", i),
			}); err != nil {
				t.Fatalf("Broadcast %d: %v", i, err)
			}
		}

		cases := []struct {
			name  string
			limit int
			want  int
		}{
			{"Zero", 0, hub.DefaultBatchLimit},
			{"Negative", -5, hub.DefaultBatchLimit},
			{"One", 1, 1},
			{"AtTheCeiling", hub.MaxBatchLimit, hub.MaxBatchLimit},
			{"OverTheCeiling", hub.MaxBatchLimit + 1, hub.MaxBatchLimit},
			{"AbsurdlyOverTheCeiling", 1 << 20, hub.MaxBatchLimit},
		}
		if len(cases) == 0 {
			t.Fatal("the clamp table is empty")
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				batch, err := h.Wait(context.Background(), b, 0, c.limit, 5*time.Second)
				if err != nil {
					t.Fatalf("Wait(limit=%d): %v", c.limit, err)
				}
				if len(batch.Messages) != c.want {
					t.Fatalf("Wait(limit=%d) returned %d messages, want %d", c.limit, len(batch.Messages), c.want)
				}
				if !batch.More {
					t.Fatalf("Wait(limit=%d) cut the batch at %d of %d but did not report More", c.limit, len(batch.Messages), total)
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// POLL-2 — the wake, and its ORDERING against the durable write
// ---------------------------------------------------------------------------

func TestWaiterWakeup(t *testing.T) {
	t.Run("AParkedWaiterIsWokenByABroadcast", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		const timeout = 10 * time.Second
		type outcome struct {
			batch   hub.Batch
			err     error
			elapsed time.Duration
		}
		out := make(chan outcome, 1)
		go func() {
			start := time.Now()
			batch, err := h.Wait(context.Background(), b, 0, 10, timeout)
			out <- outcome{batch, err, time.Since(start)}
		}()

		waitForWaiters(t, h, 1, "the waiter must be parked before the broadcast")

		res, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("wake up"), IdempotencyKey: "k-wake"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}

		select {
		case got := <-out:
			if got.err != nil {
				t.Fatalf("Wait: %v", got.err)
			}
			if got.batch.TimedOut {
				t.Fatal("the woken waiter reported TimedOut")
			}
			if len(got.batch.Messages) != 1 || got.batch.Messages[0].ID != res.MessageID {
				t.Fatalf("the woken waiter got %v, want exactly %s", got.batch.Messages, res.MessageID)
			}
			if got.elapsed > timeout/2 {
				t.Fatalf("the waiter took %v of its %v timeout; it was not woken, it nearly expired", got.elapsed, timeout)
			}
		case <-time.After(timeout):
			t.Fatal("a parked waiter was never woken by a broadcast")
		}
	})

	t.Run("AWaiterIsNotWokenByAMessageItCannotSee", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta", "gamma")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")
		g := agentID(t, h.BusID(), "gamma")

		const (
			after   = uint64(0)
			timeout = 400 * time.Millisecond
		)
		type outcome struct {
			batch hub.Batch
			err   error
		}
		out := make(chan outcome, 1)
		go func() {
			batch, err := h.Wait(context.Background(), g, after, 10, timeout)
			out <- outcome{batch, err}
		}()

		waitForWaiters(t, h, 1, "gamma must be parked before the DM")

		if _, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: b, Body: []byte("not for gamma"), IdempotencyKey: "k-notseen"}); err != nil {
			t.Fatalf("Send: %v", err)
		}

		select {
		case got := <-out:
			if got.err != nil {
				t.Fatalf("Wait: %v", got.err)
			}
			if !got.batch.TimedOut {
				t.Fatal("gamma's poll returned before its deadline; it was woken by a message it is not entitled to see")
			}
			if len(got.batch.Messages) != 0 {
				t.Fatalf("gamma received %v — a DM addressed to beta", got.batch.Messages)
			}
			if got.batch.Cursor != after {
				t.Fatalf("gamma's cursor moved from %d to %d without a delivery", after, got.batch.Cursor)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("gamma's poll never returned")
		}
	})

	t.Run("TheWakeHappensOnlyAfterTheWriteIsDurable", func(t *testing.T) {
		// THE ORDERING PROPERTY. publish must be
		//     durable write -> apply to the serving copy -> wake
		// and never wake first: a waiter released before the commit is fsynced
		// can observe a message a crash then un-observes (invariants 4 and 5).
		//
		// probeLog.onWrite runs after wal.Write has committed and fsynced and
		// BEFORE publish continues, so at that instant no waiter may yet have
		// been released. Both facts are recorded and compared afterwards; the
		// check inside onWrite cannot call t.Fatal (wrong goroutine's stack for
		// FailNow) so it records instead.
		dir := t.TempDir()
		lg := openTestLog(t, dir, true)

		var mu sync.Mutex
		var (
			writeReturned  time.Time
			releasedAt     time.Time
			releasedEarly  bool
			onWriteRanOnce bool
		)

		probe := &probeLog{inner: lg}
		probe.onWrite = func() {
			mu.Lock()
			defer mu.Unlock()
			onWriteRanOnce = true
			if !releasedAt.IsZero() {
				releasedEarly = true
			}
			writeReturned = time.Now()
		}

		h := newHubOverDurable(t, probe, lg, testBusID, "alpha", "beta")
		a := agentID(t, testBusID, "alpha")
		b := agentID(t, testBusID, "beta")

		done := make(chan hub.Batch, 1)
		go func() {
			batch, err := h.Wait(context.Background(), b, 0, 10, 10*time.Second)
			mu.Lock()
			releasedAt = time.Now()
			mu.Unlock()
			if err != nil {
				panic(fmt.Sprintf("Wait in the ordering probe: %v", err))
			}
			done <- batch
		}()

		waitForWaiters(t, h, 1, "the waiter must be parked before the durable write starts")

		res, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("ordered"), IdempotencyKey: "k-order"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}

		select {
		case batch := <-done:
			if len(batch.Messages) != 1 || batch.Messages[0].ID != res.MessageID {
				t.Fatalf("the woken waiter got %v, want %s", batch.Messages, res.MessageID)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the waiter was never woken")
		}

		mu.Lock()
		defer mu.Unlock()
		if !onWriteRanOnce {
			t.Fatal("probeLog.onWrite never ran, so the ordering was never observed — this subtest proved nothing")
		}
		if releasedEarly {
			t.Fatal("a parked waiter had ALREADY been released when the durable write returned: the wake ran before the commit was fsynced (invariant 4)")
		}
		if releasedAt.IsZero() {
			t.Fatal("the waiter never recorded a release instant")
		}
		if !releasedAt.After(writeReturned) {
			t.Fatalf("the waiter was released at %v, not after the durable write returned at %v", releasedAt, writeReturned)
		}
	})

	t.Run("TheRegistrationRaceNeverLosesAMessage", func(t *testing.T) {
		// Between Wait's first store read and its entry in the waiter map, a
		// publisher can commit. The second store read after registration is what
		// closes that window: for any message, either the append precedes the
		// second read or the notify follows the registration. Nothing else.
		//
		// Started and published with no synchronisation at all, so the schedule
		// lands all over that window across iterations.
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		const iterations = 200
		delivered := 0
		for i := 0; i < iterations; i++ {
			_, _, _, after, _ := h.Store().Stats()

			type outcome struct {
				batch hub.Batch
				err   error
			}
			out := make(chan outcome, 1)
			go func() {
				batch, err := h.Wait(context.Background(), b, after, 16, 10*time.Second)
				out <- outcome{batch, err}
			}()

			res, err := mintedBroadcast(t, h, hub.BroadcastRequest{
				Sender:         a,
				Body:           []byte(fmt.Sprintf("race-%d", i)),
				IdempotencyKey: fmt.Sprintf("k-race-%d", i),
			})
			if err != nil {
				t.Fatalf("iteration %d: Broadcast: %v", i, err)
			}

			select {
			case got := <-out:
				if got.err != nil {
					t.Fatalf("iteration %d: Wait: %v", i, got.err)
				}
				if got.batch.TimedOut {
					t.Fatalf("iteration %d: the waiter timed out even though %s was published while it registered — the registration race lost a message", i, res.MessageID)
				}
				found := false
				for _, m := range got.batch.Messages {
					if m.ID == res.MessageID {
						found = true
					}
				}
				if !found {
					t.Fatalf("iteration %d: the waiter returned %v, which does not include %s", i, got.batch.Messages, res.MessageID)
				}
				delivered++
			case <-time.After(20 * time.Second):
				t.Fatalf("iteration %d: the waiter never returned", i)
			}
		}
		if delivered != iterations {
			t.Fatalf("only %d of %d race iterations delivered", delivered, iterations)
		}
	})
}

// ---------------------------------------------------------------------------
// POLL-3 — concurrency: no leaks, and one wake per eligible waiter
// ---------------------------------------------------------------------------

func TestPollConcurrency(t *testing.T) {
	t.Run("VanishedClientsLeakNothing", func(t *testing.T) {
		// One agent PER PARKED POLL. hub.MaxWaitersPerAgent caps a single agent at
		// 32 concurrent long polls, so parking 50 for one id would be refused with
		// ErrCapacity and this would stop being a leak test at all. The cap itself
		// is asserted in PerAgentWaiterCapFailsClosed below; here the point is
		// that a vanished client releases its waiter and its goroutine, which is a
		// per-connection property and is unchanged by how the ids are spread.
		const n = 50
		names := make([]string, 0, n)
		for i := 0; i < n; i++ {
			names = append(names, fmt.Sprintf("v%d", i))
		}
		h, _, _ := newTestHub(t, names...)

		// Settle first: the race detector and the WAL open path leave short-lived
		// goroutines about, and a baseline taken too early reads as a leak later.
		settleGoroutines()
		baseline := runtime.NumGoroutine()

		cancels := make([]context.CancelFunc, n)
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			who := agentID(t, h.BusID(), fmt.Sprintf("v%d", i))
			ctx, cancel := context.WithCancel(context.Background())
			cancels[i] = cancel
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := h.Wait(ctx, who, 0, 10, 60*time.Second); !errors.Is(err, context.Canceled) {
					errs <- fmt.Errorf("Wait returned err = %v, want context.Canceled", err)
				}
			}()
		}

		waitForWaiters(t, h, n, "every client must really park before it vanishes")

		for _, cancel := range cancels {
			cancel()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("%v", err)
		}

		if got := h.WaiterCount(); got != 0 {
			t.Fatalf("after every client vanished, WaiterCount() = %d, want 0", got)
		}

		// The runtime is not synchronous about reaping, so poll rather than
		// assert instantly. The tolerance is small on purpose: 50 leaked
		// goroutines would sail past a generous one.
		const tolerance = 5
		deadline := time.Now().Add(10 * time.Second)
		for {
			settleGoroutines()
			got := runtime.NumGoroutine()
			if got <= baseline+tolerance {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("goroutine count did not return to baseline: %d now, %d before parking %d waiters (tolerance %d)", got, baseline, n, tolerance)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("PerAgentWaiterCapFailsClosed", func(t *testing.T) {
		// hub.MaxWaitersPerAgent bounds how many long polls ONE agent may have
		// parked at once. The cost it bounds is notify's scan, which runs under
		// writeMu on the critical path of every send — so without it one agent
		// parking thousands of polls slows every OTHER agent's durable write.
		//
		// The two halves both matter. It must FAIL CLOSED (refuse the new poll),
		// and it must EVICT NOTHING: evicting would let one connection of an agent
		// kill another's, so the already-parked polls have to come back normally.
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		if hub.MaxWaitersPerAgent <= 0 {
			t.Fatalf("hub.MaxWaitersPerAgent = %d; there is no cap to test", hub.MaxWaitersPerAgent)
		}

		type outcome struct {
			batch hub.Batch
			err   error
		}
		out := make(chan outcome, hub.MaxWaitersPerAgent)
		var wg sync.WaitGroup
		for i := 0; i < hub.MaxWaitersPerAgent; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				batch, err := h.Wait(context.Background(), b, 0, 10, 20*time.Second)
				out <- outcome{batch, err}
			}()
		}
		waitForWaiters(t, h, hub.MaxWaitersPerAgent, "the agent must fill its bucket to exactly the cap")

		// One over. It is refused BEFORE it parks, so it returns immediately.
		start := time.Now()
		batch, err := h.Wait(context.Background(), b, 0, 10, 20*time.Second)
		elapsed := time.Since(start)
		if !errors.Is(err, hub.ErrCapacity) {
			t.Fatalf("the %dth concurrent poll for one agent gave err = %v, want ErrCapacity", hub.MaxWaitersPerAgent+1, err)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("the refused poll took %v; it parked instead of failing closed", elapsed)
		}
		if len(batch.Messages) != 0 || batch.TimedOut {
			t.Fatalf("a refused poll returned %+v, want an empty non-timeout batch", batch)
		}
		if batch.Cursor != 0 {
			t.Fatalf("a refused poll returned cursor %d, want the cursor it was given (0)", batch.Cursor)
		}
		// EVICTS NOTHING: the parked waiters are all still there.
		if got := h.WaiterCount(); got != hub.MaxWaitersPerAgent {
			t.Fatalf("after the refusal WaiterCount() = %d, want the %d already-parked waiters untouched", got, hub.MaxWaitersPerAgent)
		}

		// A DIFFERENT agent is unaffected — the bucket is per-agent, so a flooder
		// can only fill its own.
		if _, err := h.Wait(context.Background(), a, 0, 10, 50*time.Millisecond); err != nil {
			t.Fatalf("another agent's poll was refused while %s was at its cap: %v", b, err)
		}

		// And the already-parked waiters are UNDISTURBED: one broadcast still
		// wakes every one of them, normally.
		res, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("still working"), IdempotencyKey: "k-cap"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("the capped agent's parked waiters were never woken; %d are still parked", h.WaiterCount())
		}
		close(out)

		woken := 0
		for got := range out {
			if got.err != nil {
				t.Fatalf("a parked waiter returned err = %v after the cap was hit", got.err)
			}
			if got.batch.TimedOut {
				t.Fatal("a waiter parked before the cap was hit timed out; the refusal disturbed it")
			}
			if len(got.batch.Messages) != 1 || got.batch.Messages[0].ID != res.MessageID {
				t.Fatalf("a woken waiter got %v, want exactly %s", got.batch.Messages, res.MessageID)
			}
			woken++
		}
		if woken != hub.MaxWaitersPerAgent {
			t.Fatalf("%d of %d parked waiters were woken", woken, hub.MaxWaitersPerAgent)
		}
		if got := h.WaiterCount(); got != 0 {
			t.Fatalf("after the herd returned, WaiterCount() = %d, want 0", got)
		}
		// The bucket drains, so the cap is a CONCURRENCY bound and not a lifetime
		// quota: the same agent can poll again.
		if _, err := h.Wait(context.Background(), b, res.Seq, 10, 50*time.Millisecond); err != nil {
			t.Fatalf("after its waiters drained, %s was still refused: %v", b, err)
		}
	})

	t.Run("ThunderingHerdWakesEveryEligibleWaiterExactlyOnce", func(t *testing.T) {
		const n = 50
		names := make([]string, 0, n+1)
		names = append(names, "sender")
		for i := 0; i < n; i++ {
			names = append(names, fmt.Sprintf("w%d", i))
		}
		h, _, _ := newTestHub(t, names...)
		sender := agentID(t, h.BusID(), "sender")

		type delivery struct {
			agent string
			ids   []string
			batch hub.Batch
			err   error
		}
		results := make(chan delivery, n)

		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			who := agentID(t, h.BusID(), fmt.Sprintf("w%d", i))
			wg.Add(1)
			go func() {
				defer wg.Done()
				batch, err := h.Wait(context.Background(), who, 0, 16, 10*time.Second)
				var got []string
				for _, m := range batch.Messages {
					got = append(got, m.ID)
				}
				results <- delivery{agent: who, ids: got, batch: batch, err: err}
			}()
		}

		// The sender parks too, and must NOT be woken by its own broadcast.
		senderOut := make(chan delivery, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch, err := h.Wait(context.Background(), sender, 0, 16, 1500*time.Millisecond)
			senderOut <- delivery{agent: sender, batch: batch, err: err}
		}()

		waitForWaiters(t, h, n+1, "all waiters, including the sender's, must park before the broadcast")

		res, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: sender, Body: []byte("one to many"), IdempotencyKey: "k-herd"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("not every waiter returned; %d are still parked", h.WaiterCount())
		}
		close(results)

		perAgent := map[string]int{}
		total := 0
		checked := 0
		for d := range results {
			if d.err != nil {
				t.Fatalf("%s: Wait: %v", d.agent, d.err)
			}
			if d.batch.TimedOut {
				t.Fatalf("%s timed out; the single broadcast did not wake it", d.agent)
			}
			for _, id := range d.ids {
				if id != res.MessageID {
					t.Fatalf("%s received %s, but only %s was published", d.agent, id, res.MessageID)
				}
				perAgent[d.agent]++
				total++
			}
			checked++
		}
		if checked != n {
			t.Fatalf("collected %d waiter results, want %d", checked, n)
		}
		if len(perAgent) != n {
			t.Fatalf("%d of %d agents received the broadcast", len(perAgent), n)
		}
		for agent, count := range perAgent {
			if count != 1 {
				t.Fatalf("%s received the broadcast %d times, want exactly 1", agent, count)
			}
		}
		if total != n {
			t.Fatalf("%d total deliveries for one broadcast to %d eligible waiters, want %d", total, n, n)
		}

		sd := <-senderOut
		if sd.err != nil {
			t.Fatalf("the sender's own poll: %v", sd.err)
		}
		if !sd.batch.TimedOut || len(sd.batch.Messages) != 0 {
			t.Fatalf("the SENDER's parked poll was woken by its own broadcast: %+v", sd.batch)
		}

		if got := h.WaiterCount(); got != 0 {
			t.Fatalf("after the herd returned, WaiterCount() = %d, want 0", got)
		}
	})
}

// settleGoroutines gives the runtime a chance to reap goroutines that have
// already returned, so a leak check compares like with like.
func settleGoroutines() {
	for i := 0; i < 3; i++ {
		runtime.Gosched()
		runtime.GC()
		time.Sleep(2 * time.Millisecond)
	}
}
