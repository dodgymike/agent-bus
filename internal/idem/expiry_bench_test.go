// Benchmarks for IDEM-19: the expiry sweep's cost as a function of the number
// of RETAINED records, which is the number the defect was measured against.
//
// # What is being measured, and why the shape of the benchmark matters
//
// The sweep is on the hot path for EVERY mutating operation — Lookup, Remember,
// Recover, Admit, Full and Stats all call expireLocked first (see Store's doc),
// so its cost is paid per send, not per housekeeping tick. The pathological
// input is therefore not "a big table" but a big table with STAGGERED
// deadlines: each sweep evicts a handful of records and finds the next one
// live, so the per-call work must be proportional to what it EVICTED, never to
// what it RETAINED.
//
// BenchmarkExpireDrainStaggered drives exactly that: a full table whose records
// commit one step apart, drained one step at a time. It is the reproduction of
// the ACK-2 finding (48.4s to drain, against 32ms for the amortised form, on
// that gate's box; 62.70s against 57.11ms on this one) and
// it is deliberately reported as ns/op over the WHOLE DRAIN, so the figure is
// comparable to the finding's wall-clock number rather than to a per-call one.
//
// The fill uses Recover rather than Remember because the fair share
// (retention.go) caps any single agent at maxEntries/(agents+1) and so makes a
// genuinely FULL table unreachable through the live path by construction. A
// recovered full table is a real state, described in Recover's own doc: a log
// written before the fair share existed replays without adjudication. The
// eviction path under test is identical either way — expireLocked does not know
// which entry point inserted a record — and
// BenchmarkExpireDrainStaggeredLivePath covers the live-path fill for the same
// reason a proof should not rest on one shape.
package idem_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
)

// staggerStep is the commit-time spacing between adjacent records. It only has
// to be non-zero: what makes the input pathological is that the deadlines are
// DISTINCT, so each sweep stops at the first live record.
const staggerStep = time.Millisecond

// buildStaggeredTable fills a Store with n records committed one staggerStep
// apart, using Recover so the per-agent fair share does not cap the fill (see
// the file comment). The returned clock pointer is what the caller advances to
// drive expiry; window is the retention window in force.
func buildStaggeredTable(tb testing.TB, n int, window time.Duration) (*idem.Store, *time.Time) {
	tb.Helper()
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := base
	s := idem.NewStore(idem.StoreOptions{
		Window:     window,
		MaxEntries: n,
		Now:        func() time.Time { return clock },
	})
	for i := 0; i < n; i++ {
		rec := idem.Record{
			Agent:       fmt.Sprintf("bus1.agent-%d", i%64),
			Op:          idem.OpSend,
			Key:         fmt.Sprintf("k-%08d", i),
			Fingerprint: fp(byte(i)),
			Seq:         uint64(i + 1),
			CommittedAt: base.Add(time.Duration(i) * staggerStep),
		}
		if err := s.Recover(rec); err != nil {
			tb.Fatalf("filling table at i=%d: %v", i, err)
		}
	}
	if got := s.Stats().Count; got != n {
		tb.Fatalf("table not full: retained %d, want %d", got, n)
	}
	return s, &clock
}

// drainStaggered advances the clock one staggerStep at a time and calls Expire
// after each step, which is what a bus doing one mutating operation per step
// does implicitly. It returns when the table is empty, and fails if the drain
// did not actually evict everything — a benchmark that measured an empty loop
// would be the vacuous case CLAUDE.md warns about.
func drainStaggered(tb testing.TB, s *idem.Store, clock *time.Time, n int, window time.Duration) {
	tb.Helper()
	for i := 0; i <= n; i++ {
		*clock = clock.Add(staggerStep)
		s.Expire()
	}
	if got := s.Stats().Count; got != 0 {
		tb.Fatalf("drain left %d records retained, want 0", got)
	}
}

// BenchmarkExpireDrainStaggered is the IDEM-19 reproduction. Each iteration
// builds a full table of n records with staggered deadlines (NOT timed) and
// then drains it one deadline at a time (timed). With an O(retained)
// compaction the drain is quadratic in n; with an amortised one it is linear,
// and the ratio between the n=4096 and n=65536 rows is what shows which.
func BenchmarkExpireDrainStaggered(b *testing.B) {
	window := time.Hour
	for _, n := range []int{1024, 4096, 16384, 65536} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				s, clock := buildStaggeredTable(b, n, window)
				// Advance to the instant the OLDEST record falls out of the
				// window, so the very first timed Expire evicts rather than
				// walking a table that is entirely live.
				*clock = clock.Add(window)
				b.StartTimer()
				drainStaggered(b, s, clock, n, window)
			}
		})
	}
}

// BenchmarkExpireDrainStaggeredLivePath is the same drain over a table filled
// through Remember — the live path, fair share and all. It cannot reach a full
// table (that is the fair share working as designed), so it fills to just under
// the pressure line across many agents; the point is that the sweep cost has
// the same shape whichever entry point inserted the records.
func BenchmarkExpireDrainStaggeredLivePath(b *testing.B) {
	window := time.Hour
	const maxEntries = 65536
	// Under the pressure line (maxEntries/2) the share is not enforced at all,
	// so this fill is refusal-free by construction.
	n := maxEntries/2 - 1
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		clock := base
		s := idem.NewStore(idem.StoreOptions{
			Window:     window,
			MaxEntries: maxEntries,
			Now:        func() time.Time { return clock },
		})
		for j := 0; j < n; j++ {
			rec := idem.Record{
				Agent:       fmt.Sprintf("bus1.agent-%d", j%64),
				Op:          idem.OpSend,
				Key:         fmt.Sprintf("k-%08d", j),
				Fingerprint: fp(byte(j)),
				Seq:         uint64(j + 1),
				CommittedAt: base.Add(time.Duration(j) * staggerStep),
			}
			if err := s.Remember(rec); err != nil {
				b.Fatalf("live-path fill at j=%d: %v", j, err)
			}
		}
		// Advance to the instant the OLDEST record falls out of the window,
		// so the drain proceeds one staggered deadline at a time rather than
		// dropping the whole table in a single call.
		clock = clock.Add(window)
		b.StartTimer()
		drainStaggered(b, s, &clock, n, window)
	}
}
