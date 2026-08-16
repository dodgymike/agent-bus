// Benchmarks for IDEM-19's internal/ack half: the sweep's cost as a function of
// how many rows are RETAINED, which is the number the defect was measured
// against.
//
// # Why TestSweepIsNotOccupancyLinear did not catch this
//
// That guard exists for exactly this defect class and it passes, because it
// asserts sweptEntries == 0 — it only ever exercises the case where NOTHING has
// expired. With nothing expired sweepLocked's pop loop breaks immediately,
// `drop == 0` returns early, and the compaction below it never runs at all. The
// expensive path is the one where a sweep pops a FEW entries and then copies the
// whole surviving tail, which is what STAGGERED deadlines produce and what a
// busy bus produces naturally.
//
// So the guard measured the pop loop, which was already O(expired), and missed
// the tail beside it. sweptEntries counts POPS, and the copy is not a pop. That
// is why the package could document "IT IS O(EXPIRED), NOT O(RETAINED)" in
// capitals while not being it.
//
// # Why this site is worse than internal/idem's
//
// Every exported entry point sweeps and one production Accept sweeps THREE
// times (its own, Apply's during the live wal write, and foldIn's), all inside
// Hub.publish with the GLOBAL WRITE LOCK held — so the cost is paid by every
// writer on the bus, not just by the caller. And the queue holds up to two
// entries per row (one on insert, one when the anchor moves at settle), bounded
// at 2*maxEntries = 131072 LIVE entries, so a single sweep can copy twice what
// internal/idem's did.
package ack

import (
	"fmt"
	"testing"
	"time"
)

// staggerStep is the spacing between adjacent rows' deadlines. It only has to
// be non-zero: what makes the input pathological is that the deadlines are
// DISTINCT, so each sweep stops at the first live entry after popping a few.
const staggerStep = time.Millisecond

// buildStaggeredAckTable fills a Store with n accepted rows whose retention
// deadlines are one staggerStep apart. The returned pointer is the clock the
// caller advances to drive the sweep.
//
// maxEntries is set to 4n so the fill stays well under the pressure line
// (maxEntries/2) and the per-sender fair share never refuses — this measures
// the sweep, not admission.
func buildStaggeredAckTable(tb testing.TB, n int) (*Store, *time.Time, time.Time) {
	tb.Helper()
	base := testAccepted
	clock := base
	s := NewStore(Options{MaxEntries: 4 * n, Now: func() time.Time { return clock }})
	if err := s.Attach(&fakeLog{}); err != nil {
		tb.Fatalf("Attach: %v", err)
	}
	// Sequence 0 is never allocated (invariant 1), so message ids start at 1.
	for i := 1; i <= n; i++ {
		clock = base.Add(time.Duration(i) * staggerStep)
		if err := s.Accept(fmt.Sprintf("testbus-%d", i), fmt.Sprintf("testbus.s%d-1", i%64), testRecipient); err != nil {
			tb.Fatalf("Accept %d: %v", i, err)
		}
	}
	if got := s.Len(); got != n {
		tb.Fatalf("table holds %d rows, want %d", got, n)
	}
	return s, &clock, base
}

// drainStaggeredAck advances the clock one staggerStep at a time and calls Len,
// which sweeps — the same sweep every exported entry point runs. It fails if the
// drain did not actually retire everything, so the benchmark cannot silently
// measure an empty loop.
func drainStaggeredAck(tb testing.TB, s *Store, clock *time.Time, n int) {
	tb.Helper()
	for i := 0; i <= n; i++ {
		*clock = clock.Add(staggerStep)
		s.Len()
	}
	if got := s.Len(); got != 0 {
		tb.Fatalf("drain left %d rows retained, want 0", got)
	}
}

// BenchmarkAckSweepDrainStaggered is the reproduction. Each iteration builds a
// table of n rows with staggered deadlines (NOT timed) and drains it one
// deadline at a time (timed). With an O(retained) compaction the drain is
// quadratic in n; with an amortised one it is linear, and the ratio between the
// rows is what shows which.
func BenchmarkAckSweepDrainStaggered(b *testing.B) {
	for _, n := range []int{1024, 4096, 16384, 65536} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				s, clock, base := buildStaggeredAckTable(b, n)
				// Step to the instant the OLDEST row falls out of the window —
				// computed from base, NOT from the current clock. The fill
				// advances the clock to base+n*staggerStep, so clock+Retention
				// would already be past EVERY deadline and the first sweep would
				// drain the whole table in one call: linear, and measuring
				// nothing. The defect only shows when each sweep retires a FEW
				// entries and finds the next one live.
				*clock = base.Add(Retention)
				b.StartTimer()
				drainStaggeredAck(b, s, clock, n)
			}
		})
	}
}

// BenchmarkAckSweepDrainStaggeredSettled is the same drain over rows that have
// SETTLED, so every row carries TWO queue entries rather than one (§11 moves the
// anchor to SettledAt, which pushes a second entry). This is the queue at its
// documented bound of 2*maxEntries LIVE entries, and it is the shape
// internal/idem has no
// equivalent of — so the ack half needs its own measurement rather than a number
// carried across from that package.
func BenchmarkAckSweepDrainStaggeredSettled(b *testing.B) {
	const n = 16384
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		s, clock, base := buildStaggeredAckTable(b, n)
		for j := 1; j <= n; j++ {
			*clock = clock.Add(staggerStep)
			if err := s.Settle(fmt.Sprintf("testbus-%d", j), testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); err != nil {
				b.Fatalf("Settle %d: %v", j, err)
			}
		}
		// Confirm the two-entry shape this benchmark claims to measure: every
		// row must be terminal, which is what pushed the second queue entry.
		// Asserted through the exported surface so this file compiles and means
		// the same thing both before and after the fix.
		for j := 1; j <= n; j++ {
			r, ok := s.Lookup(fmt.Sprintf("testbus-%d", j), testRecipient)
			if !ok || !r.State.Terminal() {
				b.Fatalf("row %d is not terminal (found=%v), so it carries one queue entry rather than two and this is not the shape being claimed", j, ok)
			}
		}
		*clock = base.Add(Retention)
		b.StartTimer()
		drainStaggeredAck(b, s, clock, 3*n)
	}
}
