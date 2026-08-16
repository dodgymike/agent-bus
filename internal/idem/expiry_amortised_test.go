// IDEM-19's acceptance evidence: the expiry sweep costs what it EVICTED, not
// what it RETAINED — and none of invariant 10's three cases moved while it was
// made to.
//
// # Why the headline test measures ALLOCATIONS and not wall clock
//
// The defect is a performance defect, so the obvious regression test is a
// stopwatch. A stopwatch is the wrong instrument here: the threshold that
// separates 64s from 60ms on this box is a threshold that flakes on a loaded CI
// box, and a flaky perf test gets its bound loosened until it no longer
// distinguishes the two shapes at all.
//
// The ALGORITHMIC difference is directly observable instead, and it is
// deterministic. The old sweep copied the entire surviving tail into a fresh
// backing array on every call that evicted anything, so draining n staggered
// records allocated ~n^2/2 Scopes — 1.9 GB at n=8192. The amortised form copies
// each survivor a bounded number of times, so the same drain allocates under a
// megabyte. Bytes allocated does not depend on how fast or how busy the machine
// is, and the two shapes are three orders of magnitude apart, so the bound can
// be generous and still be decisive.
//
// The benchmarks in expiry_bench_test.go carry the wall-clock numbers; this file
// is what goes RED if the unconditional compaction comes back.
package idem_test

import (
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
)

// staggeredStore builds a Store holding n records committed one millisecond
// apart, through Recover (the fair share caps any one agent, and this is about
// the sweep, not about admission). The returned closure advances the clock.
func staggeredStore(tb testing.TB, n int, window time.Duration) (*idem.Store, func(time.Duration), time.Time) {
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
			CommittedAt: base.Add(time.Duration(i) * time.Millisecond),
		}
		if err := s.Recover(rec); err != nil {
			tb.Fatalf("filling the staggered table at i=%d: %v", i, err)
		}
	}
	if got := s.Stats().Count; got != n {
		tb.Fatalf("staggered table retained %d records, want %d", got, n)
	}
	return s, func(d time.Duration) { clock = clock.Add(d) }, base
}

// TestExpireSweepIsAmortisedNotQuadratic is the IDEM-19 regression guard. It
// drains a full table one staggered deadline at a time — the shape a busy bus
// produces naturally, with no attacker involved — and asserts the total memory
// the drain allocated is linear-ish in the table size rather than quadratic.
//
// Reverting expireLocked to an unconditional compaction fails this by roughly
// two thousand times the bound, not by a hair.
func TestExpireSweepIsAmortisedNotQuadratic(t *testing.T) {
	const n = 8192
	window := time.Hour
	s, advance, _ := staggeredStore(t, n, window)

	// Step to the instant the oldest record falls out of the window, so the
	// first measured sweep evicts rather than walking a wholly-live table.
	advance(window)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i <= n; i++ {
		advance(time.Millisecond)
		s.Expire()
	}
	runtime.ReadMemStats(&after)

	if got := s.Stats().Count; got != 0 {
		t.Fatalf("the drain left %d records retained, want 0: this test measured a sweep that was not sweeping, which is the vacuous case", got)
	}
	if got := s.Stats().Expired; got != uint64(n) {
		t.Fatalf("the drain evicted %d records, want %d: the eviction count is what proves the measured loop did the work", got, n)
	}

	allocated := after.TotalAlloc - before.TotalAlloc

	// The bound, derived rather than picked. One Scope is 56 bytes on amd64
	// (three string headers plus a bool and padding). The quadratic form
	// allocates ~n^2/2 of them: 8192^2/2*56 ~= 1.88 GB. The amortised form
	// copies each survivor O(1) times amortised: ~2n Scopes ~= 918 KB. 32 MiB
	// sits ~35x above the amortised figure — room for allocator overhead, a
	// different word size, or a future change to Scope — and ~58x below the
	// quadratic one, so nothing about the machine can move a result across it.
	const bound = 32 << 20
	if allocated > bound {
		t.Fatalf("draining %d staggered records allocated %d bytes, want <= %d: the sweep is compacting the whole retained set on every call again, which is O(retained) per call on a path every mutating operation runs (IDEM-19)", n, allocated, bound)
	}
	t.Logf("drain of %d staggered records allocated %d bytes (bound %d)", n, allocated, bound)
}

// TestExpireSweepScalesSublinearlyPerRetainedRecord states the same property in
// the dimension the defect was reported in: quadrupling the RETAINED set must
// not multiply the drain's cost by sixteen. It compares allocation per record
// at two sizes, so the assertion is about the SHAPE of the curve and cannot be
// satisfied by a machine that is merely fast.
func TestExpireSweepScalesSublinearlyPerRetainedRecord(t *testing.T) {
	window := time.Hour
	perRecord := func(n int) float64 {
		s, advance, _ := staggeredStore(t, n, window)
		advance(window)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for i := 0; i <= n; i++ {
			advance(time.Millisecond)
			s.Expire()
		}
		runtime.ReadMemStats(&after)
		if got := s.Stats().Count; got != 0 {
			t.Fatalf("drain at n=%d left %d retained, want 0", n, got)
		}
		return float64(after.TotalAlloc-before.TotalAlloc) / float64(n)
	}

	small := perRecord(2048)
	large := perRecord(8192)

	// Under the quadratic form the per-record cost grows with n, so this ratio
	// is ~4 when the table quadruples. Under the amortised form it is flat, so
	// the ratio is ~1. 2.0 is comfortably between them.
	if small <= 0 {
		t.Fatalf("the n=2048 drain allocated nothing at all (%.1f bytes/record): the measurement did not observe the work it is comparing against, so the ratio below would be meaningless", small)
	}
	if large/small > 2.0 {
		t.Fatalf("per-record drain cost grew %.2fx when the retained set quadrupled (%.1f -> %.1f bytes/record): the sweep's cost still scales with what it RETAINED rather than with what it EVICTED (IDEM-19)", large/small, small, large)
	}
	t.Logf("per-record drain cost: n=2048 %.1f B/record, n=8192 %.1f B/record (ratio %.2fx)", small, large, large/small)
}

// TestStatsOldestTracksTheQueueFrontNotTheArrayStart pins the bug the head
// index makes reachable and the one that would be silent: with a dead prefix
// outstanding, order[0] names an EVICTED scope. Stats must read order[head].
//
// This fails loudly (a wrong Oldest, or a zero one) against a Stats that was not
// updated alongside the sweep, which is exactly the kind of drift a queue front
// invites.
func TestStatsOldestTracksTheQueueFrontNotTheArrayStart(t *testing.T) {
	const n = 64
	window := time.Hour
	s, advance, base := staggeredStore(t, n, window)

	// Evict exactly the first record. One eviction out of 64 leaves the dead
	// prefix far smaller than the live suffix, so NO compaction is due and the
	// queue front is genuinely offset from the array start.
	advance(window + time.Millisecond/2)
	s.Expire()

	st := s.Stats()
	if st.Count != n-1 {
		t.Fatalf("retained %d records after evicting one of %d, want %d", st.Count, n, n-1)
	}
	wantOldest := base.Add(time.Millisecond)
	if !st.Oldest.Equal(wantOldest) {
		t.Fatalf("Stats().Oldest is %v, want %v (the second record, now at the queue front): Stats is reading the array start, where an evicted scope lives, instead of the queue front", st.Oldest, wantOldest)
	}
	if st.OldestAge <= 0 {
		t.Fatalf("Stats().OldestAge is %v, want positive: a zero age is what a lookup of an evicted scope produces, and it would make the retention margin look unused when it is not", st.OldestAge)
	}
}

// TestExpiryOrderIsStillOldestFirstWhenAppendingIntoADeadPrefix exercises the
// interleaving the head index makes possible: remembering NEW records while a
// dead prefix is outstanding, then evicting again. Eviction must still proceed
// strictly oldest-first, and must never evict a record newer than one it kept.
func TestExpiryOrderIsStillOldestFirstWhenAppendingIntoADeadPrefix(t *testing.T) {
	window := time.Hour
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := base
	s := idem.NewStore(idem.StoreOptions{
		Window:     window,
		MaxEntries: 1024,
		Now:        func() time.Time { return clock },
	})

	remember := func(i int, at time.Time) {
		t.Helper()
		rec := idem.Record{
			Agent:       testAgent,
			Op:          idem.OpSend,
			Key:         fmt.Sprintf("k-%04d", i),
			Fingerprint: fp(byte(i)),
			Result:      json.RawMessage(`{"message_id":"bus1-1"}`),
			Seq:         uint64(i + 1),
			CommittedAt: at,
		}
		if err := s.Remember(rec); err != nil {
			t.Fatalf("remembering record %d: %v", i, err)
		}
	}
	present := func(i int) bool {
		t.Helper()
		sc, err := idem.NewAgentScope(testAgent, idem.OpSend, fmt.Sprintf("k-%04d", i))
		if err != nil {
			t.Fatalf("building scope %d: %v", i, err)
		}
		_, outcome := s.Lookup(sc, fp(byte(i)))
		return outcome != idem.OutcomeNew
	}

	// Ten records, one millisecond apart.
	for i := 0; i < 10; i++ {
		remember(i, base.Add(time.Duration(i)*time.Millisecond))
	}
	// Evict the first three, leaving an uncompacted dead prefix.
	clock = base.Add(window + 2*time.Millisecond + time.Millisecond/2)
	s.Expire()
	for i := 0; i < 3; i++ {
		if present(i) {
			t.Fatalf("record %d is past the window but still retained", i)
		}
	}
	for i := 3; i < 10; i++ {
		if !present(i) {
			t.Fatalf("record %d is inside the window but was evicted: eviction is not stopping at the first live record", i)
		}
	}

	// Append INTO the dead prefix's shadow: new records at the current clock.
	for i := 10; i < 15; i++ {
		remember(i, clock)
	}

	// Now age past the remaining originals but not past the new ones.
	clock = clock.Add(window - 2*time.Millisecond)
	s.Expire()
	for i := 3; i < 10; i++ {
		if present(i) {
			t.Fatalf("record %d is past the window but still retained after the second sweep", i)
		}
	}
	for i := 10; i < 15; i++ {
		if !present(i) {
			t.Fatalf("record %d was appended after the dead prefix formed and is still inside its window, but was evicted: the queue front is being computed against the wrong array offset", i)
		}
	}
}

// TestInvariantTenThreeCasesSurviveADeadPrefix is the invariant check, not the
// performance one. A sweep change must not alter which of invariant 10's three
// cases an input falls into, so all three are adjudicated with a dead prefix
// outstanding and asserted to be exactly what they were before it formed.
//
//   - same key + same payload  -> OutcomeRetry, and the ORIGINAL result verbatim
//   - same key + different payload -> OutcomeViolation
//   - unseen key -> OutcomeNew
func TestInvariantTenThreeCasesSurviveADeadPrefix(t *testing.T) {
	window := time.Hour
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := base
	s := idem.NewStore(idem.StoreOptions{
		Window:     window,
		MaxEntries: 1024,
		Now:        func() time.Time { return clock },
	})

	original := json.RawMessage(`{"message_id":"bus1-42"}`)
	// Filler records that will be evicted, forming the dead prefix.
	for i := 0; i < 8; i++ {
		rec := mustRecord(t, fmt.Sprintf("filler-%d", i), fp(byte(i)), base.Add(time.Duration(i)*time.Millisecond))
		if err := s.Remember(rec); err != nil {
			t.Fatalf("remembering filler %d: %v", i, err)
		}
	}
	// The record under test, committed well after the fillers.
	subject := idem.Record{
		Agent:       testAgent,
		Op:          idem.OpSend,
		Key:         "subject-key",
		Fingerprint: fp(200),
		Result:      original,
		Seq:         42,
		CommittedAt: base.Add(time.Minute),
	}
	if err := s.Remember(subject); err != nil {
		t.Fatalf("remembering the subject record: %v", err)
	}

	subjectScope, err := idem.NewAgentScope(testAgent, idem.OpSend, "subject-key")
	if err != nil {
		t.Fatalf("building the subject scope: %v", err)
	}
	unseenScope, err := idem.NewAgentScope(testAgent, idem.OpSend, "never-seen-key")
	if err != nil {
		t.Fatalf("building the unseen scope: %v", err)
	}

	assertThreeCases := func(stage string) {
		t.Helper()
		got, outcome := s.Lookup(subjectScope, fp(200))
		if outcome != idem.OutcomeRetry {
			t.Fatalf("%s: same key + same payload gave %v, want OutcomeRetry: a legitimate retry must replay the original result, not be re-applied or refused (invariant 10)", stage, outcome)
		}
		if string(got.Result) != string(original) {
			t.Fatalf("%s: the retry replayed result %q, want the original %q verbatim", stage, got.Result, original)
		}
		if got.Seq != 42 {
			t.Fatalf("%s: the retry replayed seq %d, want the original 42", stage, got.Seq)
		}
		if _, outcome := s.Lookup(subjectScope, fp(201)); outcome != idem.OutcomeViolation {
			t.Fatalf("%s: same key + DIFFERENT payload gave %v, want OutcomeViolation: reusing a key for new content is a protocol violation and must be refused, never collapsed into a retry (invariant 10)", stage, outcome)
		}
		if _, outcome := s.Lookup(unseenScope, fp(7)); outcome != idem.OutcomeNew {
			t.Fatalf("%s: an unseen key gave %v, want OutcomeNew", stage, outcome)
		}
	}

	assertThreeCases("with no dead prefix")

	// Age past every filler but not past the subject, so a dead prefix of eight
	// evicted slots sits in front of the live record being adjudicated.
	clock = base.Add(window + 30*time.Second)
	s.Expire()
	if st := s.Stats(); st.Count != 1 || st.Expired != 8 {
		t.Fatalf("after the sweep: retained %d (want 1), evicted %d (want 8) — the dead prefix this test needs did not form", st.Count, st.Expired)
	}

	assertThreeCases("with an eight-slot dead prefix outstanding")
}

// TestRestartWithADeadPrefixRebuildsTheSameIdempotencyState is the durability
// half of invariant 10 — "the server durably remembers which keys it has
// already applied, and that memory survives restart" — checked against the one
// thing IDEM-19 added: a queue front that exists only in memory.
//
// The real SIGKILL evidence is TestIdemCrashInjectionRestart* in
// crashinjection_test.go, which this does not replace. What it adds is the state
// those tests cannot easily arrange: a store that dies with a DEAD PREFIX
// OUTSTANDING. head is in-memory bookkeeping and is not written anywhere, so the
// property to prove is that it cannot survive into the rebuilt table and cannot
// change what that table answers. A rebuilt store must adjudicate all three of
// invariant 10's cases identically to the pre-restart one.
func TestRestartWithADeadPrefixRebuildsTheSameIdempotencyState(t *testing.T) {
	window := time.Hour
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := base
	newStore := func() *idem.Store {
		return idem.NewStore(idem.StoreOptions{
			Window:     window,
			MaxEntries: 1024,
			Now:        func() time.Time { return clock },
		})
	}

	// The records the pre-restart bus accepted: 8 that will age out, and 4 that
	// will still be live when it dies.
	type accepted struct {
		key    string
		fpByte byte
		at     time.Time
		result json.RawMessage
	}
	var all []accepted
	for i := 0; i < 8; i++ {
		all = append(all, accepted{
			key:    fmt.Sprintf("doomed-%d", i),
			fpByte: byte(i),
			at:     base.Add(time.Duration(i) * time.Millisecond),
			result: json.RawMessage(fmt.Sprintf(`{"message_id":"bus1-%d"}`, i)),
		})
	}
	for i := 0; i < 4; i++ {
		all = append(all, accepted{
			key:    fmt.Sprintf("survivor-%d", i),
			fpByte: byte(100 + i),
			at:     base.Add(time.Minute + time.Duration(i)*time.Millisecond),
			result: json.RawMessage(fmt.Sprintf(`{"message_id":"bus1-%d"}`, 100+i)),
		})
	}

	live := newStore()
	for _, a := range all {
		rec := idem.Record{
			Agent:       testAgent,
			Op:          idem.OpSend,
			Key:         a.key,
			Fingerprint: fp(a.fpByte),
			Result:      a.result,
			Seq:         uint64(a.fpByte) + 1,
			CommittedAt: a.at,
		}
		if err := live.Remember(rec); err != nil {
			t.Fatalf("pre-restart Remember of %s: %v", a.key, err)
		}
	}

	// Age past the doomed records only. The dead prefix is now 8 slots against
	// 4 live ones, so a compaction IS due at the next sweep — which is exactly
	// why this is the interesting restart point: the in-memory queue is at its
	// most rearranged.
	clock = base.Add(window + 30*time.Second)
	live.Expire()
	if st := live.Stats(); st.Count != 4 || st.Expired != 8 {
		t.Fatalf("pre-restart: retained %d (want 4), evicted %d (want 8)", st.Count, st.Expired)
	}

	// THE RESTART. A fresh store rebuilt through Recover from the records the
	// log still holds — which is the survivors: an expired record's transaction
	// is still in the log, but replay re-derives the window with the same pure
	// predicate, so the rebuilt table holds what the live one held.
	rebuilt := newStore()
	for _, a := range all {
		rec := idem.Record{
			Agent:       testAgent,
			Op:          idem.OpSend,
			Key:         a.key,
			Fingerprint: fp(a.fpByte),
			Result:      a.result,
			Seq:         uint64(a.fpByte) + 1,
			CommittedAt: a.at,
		}
		if err := rebuilt.Recover(rec); err != nil {
			t.Fatalf("replaying %s: %v", a.key, err)
		}
	}

	if got, want := rebuilt.Stats().Count, live.Stats().Count; got != want {
		t.Fatalf("the rebuilt table retains %d records, the pre-restart one %d: replay did not reconstruct the same applied-key set", got, want)
	}

	// Every one of invariant 10's three cases must be answered identically by
	// both stores, for every key the bus ever accepted.
	for _, a := range all {
		sc, err := idem.NewAgentScope(testAgent, idem.OpSend, a.key)
		if err != nil {
			t.Fatalf("building scope for %s: %v", a.key, err)
		}

		// same key + same payload
		liveRec, liveOut := live.Lookup(sc, fp(a.fpByte))
		reRec, reOut := rebuilt.Lookup(sc, fp(a.fpByte))
		if liveOut != reOut {
			t.Fatalf("%s: same key + same payload is %v before the restart and %v after: recovery changed which of invariant 10's cases this input falls into", a.key, liveOut, reOut)
		}
		if liveOut == idem.OutcomeRetry {
			if string(liveRec.Result) != string(reRec.Result) {
				t.Fatalf("%s: the replayed result is %q before the restart and %q after: a retry must get the ORIGINAL result verbatim across a restart", a.key, liveRec.Result, reRec.Result)
			}
			if liveRec.Seq != reRec.Seq {
				t.Fatalf("%s: the replayed seq is %d before the restart and %d after", a.key, liveRec.Seq, reRec.Seq)
			}
		}

		// same key + different payload
		_, liveViol := live.Lookup(sc, fp(a.fpByte^0xff))
		_, reViol := rebuilt.Lookup(sc, fp(a.fpByte^0xff))
		if liveViol != reViol {
			t.Fatalf("%s: same key + DIFFERENT payload is %v before the restart and %v after", a.key, liveViol, reViol)
		}
	}

	// And an unseen key is still new on both sides.
	unseen, err := idem.NewAgentScope(testAgent, idem.OpSend, "never-seen-at-all")
	if err != nil {
		t.Fatalf("building the unseen scope: %v", err)
	}
	if _, out := rebuilt.Lookup(unseen, fp(9)); out != idem.OutcomeNew {
		t.Fatalf("an unseen key is %v after the restart, want OutcomeNew", out)
	}
}
