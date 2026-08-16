// IDEM-19's acceptance evidence for internal/ack: the sweep costs what it
// POPPED, not what it RETAINED — and none of §8/§11's lifecycle semantics moved
// while it was made to.
//
// # Why these are allocation assertions and not a stopwatch
//
// The defect is a performance defect, so the obvious guard is a timer. A timer
// is the wrong instrument: the threshold separating 50s from 27ms on this box is
// one that flakes on a loaded CI box, and a flaky perf test gets loosened until
// it stops distinguishing the two shapes.
//
// The ALGORITHMIC difference is directly observable and deterministic. The old
// sweep copied the whole surviving tail on every call that popped anything, so
// draining n staggered rows allocated ~n^2/2 entries. The amortised form copies
// each survivor a bounded number of times. Bytes allocated does not depend on
// machine speed, and the two shapes are orders of magnitude apart, so the bound
// can be generous and still decisive.
//
// # The guard that was already here, and why it passed anyway
//
// TestSweepIsNotOccupancyLinear exists for this exact defect class and passed
// throughout, because it asserts sweptEntries == 0 — it only exercises the case
// where NOTHING expired, in which the pop loop breaks at once and `drop == 0`
// returns before the copy is reached. sweptEntries counts POPS; the copy is not
// a pop. These tests drive the case it does not: staggered deadlines, where each
// sweep pops a few entries and finds the next one live. That test is left
// exactly as it is — it guards the map-scan regression, which is a different
// one.
package ack

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// TestAckSweepIsAmortisedNotQuadratic is the regression guard. It drains a full
// table one staggered deadline at a time — the shape a busy bus produces
// naturally, with no attacker involved — and asserts the drain's total
// allocation is linear-ish in the table size rather than quadratic.
//
// Restoring the unconditional compaction fails this by orders of magnitude, not
// by a hair.
func TestAckSweepIsAmortisedNotQuadratic(t *testing.T) {
	const n = 8192
	s, clock, base := buildStaggeredAckTable(t, n)
	*clock = base.Add(Retention)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i <= n; i++ {
		*clock = clock.Add(staggerStep)
		s.Len()
	}
	runtime.ReadMemStats(&after)

	if got := s.Len(); got != 0 {
		t.Fatalf("the drain left %d rows retained, want 0: this measured a sweep that was not sweeping, which is the vacuous case", got)
	}
	s.mu.Lock()
	swept := s.sweptEntries
	s.mu.Unlock()
	if swept != uint64(n) {
		t.Fatalf("the drain popped %d expiry entries, want %d: the pop count is what proves the measured loop did the work", swept, n)
	}

	allocated := after.TotalAlloc - before.TotalAlloc

	// The bound, derived rather than picked. One expiryEntry is a key (two
	// string headers, 32 B) plus a time.Time (24 B) = 56 B on amd64. The
	// quadratic form allocates ~n^2/2 of them: 8192^2/2*56 ~= 1.88 GB. The
	// amortised form copies each survivor O(1) times amortised: ~2n entries
	// ~= 918 KB. 32 MiB sits ~35x above the amortised figure and ~58x below the
	// quadratic one, so no property of the machine can move a result across it.
	const bound = 32 << 20
	if allocated > bound {
		t.Fatalf("draining %d staggered rows allocated %d bytes, want <= %d: the sweep is compacting the whole retained queue on every call again, which is O(retained) on a path that runs under Hub.publish's GLOBAL WRITE LOCK (IDEM-19)", n, allocated, bound)
	}
	t.Logf("drain of %d staggered rows allocated %d bytes (bound %d)", n, allocated, bound)
}

// TestAckSweepScalesSublinearlyPerRetainedRow states the property in the
// dimension the defect was reported in: quadrupling the RETAINED set must not
// multiply the sweep cost by sixteen. Comparing allocation per row at two sizes
// asserts the SHAPE of the curve, which a merely fast machine cannot satisfy.
func TestAckSweepScalesSublinearlyPerRetainedRow(t *testing.T) {
	perRow := func(n int) float64 {
		s, clock, base := buildStaggeredAckTable(t, n)
		*clock = base.Add(Retention)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for i := 0; i <= n; i++ {
			*clock = clock.Add(staggerStep)
			s.Len()
		}
		runtime.ReadMemStats(&after)
		if got := s.Len(); got != 0 {
			t.Fatalf("drain at n=%d left %d rows, want 0", n, got)
		}
		return float64(after.TotalAlloc-before.TotalAlloc) / float64(n)
	}

	small := perRow(2048)
	large := perRow(8192)

	if small <= 0 {
		t.Fatalf("the n=2048 drain allocated nothing at all (%.1f bytes/row): the measurement did not observe the work it is comparing against, so the ratio below would be meaningless", small)
	}
	// Under the quadratic form the per-row cost grows with n, so this ratio is
	// ~4 when the table quadruples. Under the amortised form it is flat, ~1.
	if large/small > 2.0 {
		t.Fatalf("per-row drain cost grew %.2fx when the retained set quadrupled (%.1f -> %.1f bytes/row): the sweep's cost still scales with what it RETAINED rather than with what it POPPED (IDEM-19)", large/small, small, large)
	}
	t.Logf("per-row drain cost: n=2048 %.1f B/row, n=8192 %.1f B/row (ratio %.2fx)", small, large, large/small)
}

// assertExpiryQueueInvariants checks, for the CURRENT state of the store:
//
//  1. expiryHead is a valid index.
//  2. the dead prefix never exceeds the live suffix (the capacity bound).
//  3. every slot before the head is the zero expiryEntry (the memory bound).
//  4. the live region's deadlines are non-decreasing, which is what the
//     stop-at-the-first-live rule depends on.
//  5. the live queue stays within §11's 2-per-row bound.
//
// Properties 2 and 3 have no black-box signature and both fail SILENTLY — a
// store that leaked would answer every query correctly while growing — which is
// exactly the shape of bug the unconditional compaction was written to avoid.
func assertExpiryQueueInvariants(t *testing.T, s *Store, stage string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.expiryHead < 0 || s.expiryHead > len(s.expiry) {
		t.Fatalf("%s: expiryHead=%d out of range for a queue of length %d", stage, s.expiryHead, len(s.expiry))
	}
	live := len(s.expiry) - s.expiryHead
	// The guarantee compactExpiryLocked actually provides is head == 0 (it just
	// compacted, or the queue is empty) OR head < live — strictly, not merely
	// "not greater than". Asserting the real post-condition rather than a looser
	// one means a compaction that fired one entry too late is caught here rather
	// than tolerated.
	if s.expiryHead != 0 && s.expiryHead >= live {
		t.Fatalf("%s: the dead prefix (%d slots) is not strictly smaller than the live suffix (%d slots): compaction should have fired and reset the head to 0, so the deferred copy is drifting toward unbounded growth", stage, s.expiryHead, live)
	}
	for i := 0; i < s.expiryHead; i++ {
		if s.expiry[i] != (expiryEntry{}) {
			t.Fatalf("%s: popped slot %d still holds %+v, want the zero expiryEntry: the backing array is pinning a correlation key and recipient the sweep was supposed to release", stage, i, s.expiry[i])
		}
	}
	var prev time.Time
	for i := s.expiryHead; i < len(s.expiry); i++ {
		d := s.expiry[i].deadline
		if !prev.IsZero() && d.Before(prev) {
			t.Fatalf("%s: live slot %d has deadline %v, before its predecessor's %v: the queue is no longer ordered, and stop-at-the-first-live depends on that", stage, i, d, prev)
		}
		prev = d
	}
	if retained := len(s.records); live > 2*retained {
		t.Fatalf("%s: the queue holds %d LIVE entries for %d rows, want at most 2 per row (§11)", stage, live, retained)
	}
}

// TestAckExpiryQueueInvariantsHoldAcrossAStaggeredDrain drives the workload the
// defect was measured against and checks the bounds after EVERY sweep, so a
// violation is caught at the sweep that caused it.
func TestAckExpiryQueueInvariantsHoldAcrossAStaggeredDrain(t *testing.T) {
	const n = 512
	s, clock, base := buildStaggeredAckTable(t, n)
	assertExpiryQueueInvariants(t, s, "after the fill")

	*clock = base.Add(Retention)
	for i := 0; i <= n; i++ {
		*clock = clock.Add(staggerStep)
		s.Len()
		assertExpiryQueueInvariants(t, s, fmt.Sprintf("after sweep %d", i))
	}

	s.mu.Lock()
	drained, head := len(s.expiry), s.expiryHead
	s.mu.Unlock()
	if drained != 0 || head != 0 {
		t.Fatalf("after a full drain the queue has %d slots and head=%d, want 0 and 0: a fully-drained queue must release its array rather than keep a 131072-entry allocation alive on a bus that has gone quiet", drained, head)
	}

	// A bus that went quiet and then woke up: the queue is nil here, so this
	// covers the append-onto-a-released-array path that only the full-drain
	// reset reaches.
	if err := s.Accept("testbus-999999", testSender, testRecipient); err != nil {
		t.Fatalf("Accept after a full drain released the queue: %v", err)
	}
	assertExpiryQueueInvariants(t, s, "after reviving a fully-drained store")
	if _, ok := s.Lookup("testbus-999999", testRecipient); !ok {
		t.Fatalf("the row accepted after a full drain is not retained: the revived store lost a row it acknowledged")
	}
}

// TestAckExpiryQueueInvariantsHoldWhenSweepAndAdmitInterleave is the steady
// state of a busy bus — never fully drained, always both admitting and retiring
// — which is where a dead prefix persists across many operations rather than
// being cleared by a final compaction.
func TestAckExpiryQueueInvariantsHoldWhenSweepAndAdmitInterleave(t *testing.T) {
	const primed = 200
	const steps = 1200
	// MaxEntries is sized so the table never reaches its pressure line during
	// the run: the primed rows expire one per step while new ones accumulate,
	// so occupancy peaks near primed+steps. A fair-share refusal partway through
	// would end the interleaving early and quietly reduce this to a shorter
	// test than it claims to be.
	base := testAccepted
	clock := base
	s, _ := newTestStore(t, Options{MaxEntries: 20000, Now: func() time.Time { return clock }})
	for i := 1; i <= primed; i++ {
		clock = base.Add(time.Duration(i) * staggerStep)
		if err := s.Accept(fmt.Sprintf("testbus-%d", i), fmt.Sprintf("testbus.s%d-1", i%64), testRecipient); err != nil {
			t.Fatalf("priming Accept %d: %v", i, err)
		}
	}

	clock = base.Add(Retention)
	for i := primed + 1; i <= primed+steps; i++ {
		clock = clock.Add(staggerStep)
		if err := s.Accept(fmt.Sprintf("testbus-%d", i), fmt.Sprintf("testbus.s%d-1", i%64), testRecipient); err != nil {
			t.Fatalf("steady-state Accept %d: %v", i, err)
		}
		assertExpiryQueueInvariants(t, s, fmt.Sprintf("at steady-state step %d", i))
	}

	// The primed rows must actually have been retired during the run, or the
	// sweep half of "sweep and admit interleave" never happened.
	s.mu.Lock()
	swept := s.sweptEntries
	total, head := len(s.expiry), s.expiryHead
	s.mu.Unlock()
	if swept < primed {
		t.Fatalf("only %d entries were popped across %d interleaved operations, want at least %d: the sweep never engaged, so this test did not exercise the interleaving it claims", swept, steps, primed)
	}
	live := total - head
	if head > live {
		t.Fatalf("after %d steady-state operations the dead prefix is %d slots against %d live: it is accumulating instead of being compacted", steps, head, live)
	}
}

// TestAckLifecycleSemanticsSurviveADeadPrefix is the invariant check rather than
// the performance one. A sweep change must not alter what the table reports or
// which transitions it accepts, so the §8 rules are exercised with a dead prefix
// outstanding:
//
//   - a live row still reports its true state
//   - a first terminal outcome is still accepted
//   - a SECOND, DIFFERING terminal outcome is still REJECTED (§8.2 note 4)
//   - a re-presented IDENTICAL terminal outcome is still the idempotent no-op
//   - a swept row reports `unknown` (not found), which is the honest answer
func TestAckLifecycleSemanticsSurviveADeadPrefix(t *testing.T) {
	// 64 rows, of which only the first 8 are swept. Sweeping ALMOST everything
	// would be self-defeating: once the dead prefix reaches the live suffix the
	// queue compacts and head returns to 0, so the very state this test exists
	// to cover would not exist. 8 dead against 56 live keeps the prefix
	// OUTSTANDING, and the test asserts that rather than hoping for it.
	const n = 64
	const sweepFirst = 8
	s, clock, base := buildStaggeredAckTable(t, n)

	// The subject is a row in the middle: comfortably inside its window, and
	// sitting BEHIND the dead prefix in the queue.
	subject := fmt.Sprintf("testbus-%d", 40)

	assertSemantics := func(stage string) {
		t.Helper()
		r, ok := s.Lookup(subject, testRecipient)
		if !ok {
			t.Fatalf("%s: the subject row is not retained, but it is inside its window", stage)
		}
		if r.State != StateAccepted {
			t.Fatalf("%s: the subject row reports %v, want accepted", stage, r.State)
		}
	}
	assertSemantics("with no dead prefix")

	// Advance just past the 8th row's deadline, so exactly 8 entries pop.
	*clock = base.Add(Retention).Add(time.Duration(sweepFirst)*staggerStep + staggerStep/2)
	if got := s.Len(); got != n-sweepFirst {
		t.Fatalf("after the sweep %d rows are retained, want %d: the sweep did not retire what this test needs it to", got, n-sweepFirst)
	}
	s.mu.Lock()
	head, live := s.expiryHead, len(s.expiry)-s.expiryHead
	s.mu.Unlock()
	if head != sweepFirst {
		t.Fatalf("the dead prefix is %d slots, want exactly %d: this test is not exercising an OUTSTANDING prefix, which is the whole state it was written to cover", head, sweepFirst)
	}
	if head >= live {
		t.Fatalf("the dead prefix (%d) is not smaller than the live suffix (%d), so the queue would have compacted and head would be 0", head, live)
	}
	assertSemantics("with an eight-slot dead prefix outstanding")

	// A first terminal outcome is accepted.
	if err := s.Settle(subject, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("first Settle with a dead prefix outstanding: %v", err)
	}
	r, ok := s.Lookup(subject, testRecipient)
	if !ok || r.State != StateDelivered {
		t.Fatalf("after settling, the row is (%+v, %v), want delivered", r, ok)
	}

	// The IDENTICAL outcome re-presented is the idempotent no-op.
	if err := s.Settle(subject, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("re-presenting the SAME terminal outcome must be an idempotent no-op, got: %v", err)
	}

	// A DIFFERING second terminal outcome is still refused (§8.2 note 4).
	err := s.Settle(subject, testRecipient, StateRefused, ClassRecipientRefusedUndecodable, AttestedByRecipientSignatureUnverified)
	if err == nil {
		t.Fatalf("a SECOND, DIFFERING terminal outcome was accepted with a dead prefix outstanding: §8.2 note 4 requires it be rejected, and a sweep change must not have relaxed it")
	}

	// A swept row reports unknown — not found — which is the honest answer.
	if _, ok := s.Lookup("testbus-1", testRecipient); ok {
		t.Fatalf("a row past its retention window is still reported: the sweep did not retire it")
	}
}

// TestAckRestartWithADeadPrefixRebuildsTheSameState is the durability check.
// expiryHead is in-memory bookkeeping written nowhere, so the property to prove
// is that it cannot survive into a rebuilt table and cannot change what that
// table reports. A store rebuilt from the log alone must agree with the live one
// row for row.
func TestAckRestartWithADeadPrefixRebuildsTheSameState(t *testing.T) {
	base := testAccepted
	clock := base
	s, lg := newTestStore(t, Options{MaxEntries: 4096, Now: func() time.Time { return clock }})

	// Rows that will be swept, then rows that will survive.
	for i := 1; i <= 16; i++ {
		clock = base.Add(time.Duration(i) * staggerStep)
		if err := s.Accept(fmt.Sprintf("testbus-%d", i), testSender, testRecipient); err != nil {
			t.Fatalf("Accept doomed %d: %v", i, err)
		}
	}
	for i := 1; i <= 4; i++ {
		clock = base.Add(time.Hour + time.Duration(i)*staggerStep)
		if err := s.Accept(fmt.Sprintf("testbus-%d", 1000+i), testSender, testRecipient); err != nil {
			t.Fatalf("Accept survivor %d: %v", i, err)
		}
	}
	// Settle one survivor so the rebuilt table must agree about a terminal row
	// too, not just about existence.
	if err := s.Settle("testbus-1001", testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("Settle survivor: %v", err)
	}

	// Sweep the doomed rows out, forming a dead prefix.
	clock = base.Add(Retention).Add(20 * staggerStep)
	if got := s.Len(); got != 4 {
		t.Fatalf("before the restart %d rows are retained, want 4", got)
	}

	// THE RESTART: rebuild from the log alone, at the same clock.
	rebuilt := lg.replayFrom(t, Options{MaxEntries: 4096, Now: func() time.Time { return clock }})

	if got, want := rebuilt.Len(), s.Len(); got != want {
		t.Fatalf("the rebuilt table holds %d rows, the live one %d: replay did not reconstruct the same set", got, want)
	}
	for i := 1; i <= 4; i++ {
		k := fmt.Sprintf("testbus-%d", 1000+i)
		liveRec, liveOK := s.Lookup(k, testRecipient)
		reRec, reOK := rebuilt.Lookup(k, testRecipient)
		if liveOK != reOK {
			t.Fatalf("%s: retained=%v before the restart and %v after", k, liveOK, reOK)
		}
		if liveRec.State != reRec.State {
			t.Fatalf("%s: state is %v before the restart and %v after: recovery changed what the bus reports about a delivery", k, liveRec.State, reRec.State)
		}
		if liveRec.Class != reRec.Class {
			t.Fatalf("%s: class is %q before the restart and %q after", k, liveRec.Class, reRec.Class)
		}
	}
	// And a swept row is unknown on both sides.
	if _, ok := rebuilt.Lookup("testbus-1", testRecipient); ok {
		t.Fatalf("a row swept before the restart is retained after it: replay re-derives the window with the same predicate and must agree")
	}
}
