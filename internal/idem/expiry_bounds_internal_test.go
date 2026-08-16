// The two bounds compactOrderLocked's doc claims, asserted against the actual
// slice rather than against the comment.
//
// This is the package's only white-box test file, and the exception is
// deliberate. Deferring the compaction (IDEM-19) trades a copy for a dead
// prefix, and the whole safety of that trade rests on two properties that have
// NO black-box signature: the prefix stays bounded, and a vacated slot holds no
// Scope. Both fail SILENTLY — a store that leaked would serve every request
// correctly while growing, which is precisely the shape of bug the unconditional
// compaction was written to avoid in the first place. A comment asserting them
// is not evidence.
package idem

import (
	"fmt"
	"testing"
	"time"
)

// assertOrderInvariants checks, for the CURRENT state of the store:
//
//  1. head is a valid index into order.
//  2. the dead prefix never exceeds the live suffix (the capacity bound).
//  3. every slot before head is the zero Scope (the memory bound).
//  4. every live slot has a record, and they are in non-decreasing commit order.
func assertOrderInvariants(t *testing.T, s *Store, stage string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.head < 0 || s.head > len(s.order) {
		t.Fatalf("%s: head=%d is out of range for order of length %d", stage, s.head, len(s.order))
	}
	live := len(s.order) - s.head
	if s.head > live {
		t.Fatalf("%s: the dead prefix (%d slots) exceeds the live suffix (%d slots): compaction is not keeping order's length under twice the retained count, so the deferred copy has become unbounded growth", stage, s.head, live)
	}
	for i := 0; i < s.head; i++ {
		if s.order[i] != (Scope{}) {
			t.Fatalf("%s: evicted slot %d still holds scope %+v, want the zero Scope: the backing array is pinning the strings of an evicted record, which is the retain-forever leak the compaction existed to prevent", stage, i, s.order[i])
		}
	}
	if live != len(s.records) {
		t.Fatalf("%s: order holds %d live slots but records holds %d entries: the queue and the map have drifted", stage, live, len(s.records))
	}

	// THE PER-AGENT QUOTA COUNTERS, RECOUNTED FROM SCRATCH over the live region
	// and compared to what the store believes.
	//
	// This is the anti-starvation accounting, and it is the reason a sweep
	// change gets this level of scrutiny: byAgent is the fair share's only
	// input, and it is decremented inside the eviction loop the head index was
	// threaded through. A counter that OUTLIVES its records refuses an honest
	// agent for keys that no longer exist — which is the shape of the
	// RELAY-FU-IDEM-METER-BY-PEER P0, where one peer could lock out every agent
	// — and one that UNDER-counts hands an agent more than its share. Both
	// would be invisible to every other assertion here.
	wantByAgent := make(map[string]int)
	wantBuckets := 0
	for i := s.head; i < len(s.order); i++ {
		sc := s.order[i]
		if sc.enrolBusWide {
			continue
		}
		wantBuckets++
		bucket, ok := s.buckets[sc]
		if !ok {
			t.Fatalf("%s: live slot %d has no bucket recorded, so its holder cannot be metered", stage, i)
		}
		wantByAgent[bucket]++
	}
	if len(s.buckets) != wantBuckets {
		t.Fatalf("%s: buckets holds %d entries but only %d live records need one: an evicted scope's bucket was not released, so the map grows one entry per record ever accepted", stage, len(s.buckets), wantBuckets)
	}
	if len(s.byAgent) != len(wantByAgent) {
		t.Fatalf("%s: byAgent tracks %d agents, but only %d hold a live record: a counter that outlives its records refuses an honest agent for keys that no longer exist", stage, len(s.byAgent), len(wantByAgent))
	}
	for bucket, want := range wantByAgent {
		if got := s.byAgent[bucket]; got != want {
			t.Fatalf("%s: byAgent[%q] is %d, but %d live records are held by it: the fair share is being computed from a drifted counter", stage, bucket, got, want)
		}
	}
	var prev time.Time
	for i := s.head; i < len(s.order); i++ {
		r, ok := s.records[s.order[i]]
		if !ok {
			t.Fatalf("%s: live slot %d names scope %+v, which has no record", stage, i, s.order[i])
		}
		if !prev.IsZero() && r.CommittedAt.Before(prev) {
			t.Fatalf("%s: live slot %d commits at %v, before its predecessor's %v: order is no longer sorted by commit time, and expiry's stop-at-the-first-live-record rule depends on that", stage, i, r.CommittedAt, prev)
		}
		prev = r.CommittedAt
	}
}

// TestOrderInvariantsHoldAcrossAStaggeredDrain drives the exact workload the
// defect was measured against and checks the bounds after EVERY sweep, so a
// violation is caught at the sweep that caused it rather than at the end.
func TestOrderInvariantsHoldAcrossAStaggeredDrain(t *testing.T) {
	const n = 512
	window := time.Hour
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := base
	s := NewStore(StoreOptions{
		Window:     window,
		MaxEntries: n,
		Now:        func() time.Time { return clock },
	})

	for i := 0; i < n; i++ {
		rec := Record{
			Agent:       fmt.Sprintf("bus1.agent-%d", i%16),
			Op:          OpSend,
			Key:         fmt.Sprintf("k-%06d", i),
			Seq:         uint64(i + 1),
			CommittedAt: base.Add(time.Duration(i) * time.Millisecond),
		}
		rec.Fingerprint[0] = byte(i)
		if err := s.Recover(rec); err != nil {
			t.Fatalf("filling at i=%d: %v", i, err)
		}
	}
	assertOrderInvariants(t, s, "after the fill")

	clock = clock.Add(window)
	maxLen := 0
	for i := 0; i <= n; i++ {
		clock = clock.Add(time.Millisecond)
		s.Expire()
		assertOrderInvariants(t, s, fmt.Sprintf("after sweep %d", i))
		s.mu.Lock()
		if len(s.order) > maxLen {
			maxLen = len(s.order)
		}
		s.mu.Unlock()
	}

	// The array never had to grow beyond what it started with: the drain only
	// removes, so this pins that the deferred compaction did not accumulate.
	if maxLen > n {
		t.Fatalf("order grew to %d slots during a drain of %d records, want <= %d", maxLen, n, n)
	}
	s.mu.Lock()
	drained := len(s.order)
	head := s.head
	s.mu.Unlock()
	if drained != 0 || head != 0 {
		t.Fatalf("after a full drain order has %d slots and head=%d, want 0 and 0: a fully-drained store must release the backing array rather than keep a maxEntries-sized allocation alive on a bus that has gone quiet", drained, head)
	}

	// A bus that went quiet and then woke up. order is nil at this point, so
	// this covers the append-onto-a-released-array path — which is exactly the
	// state the full-drain reset creates and nothing else reaches.
	revived := Record{
		Agent:       "bus1.agent-0",
		Op:          OpSend,
		Key:         "after-the-quiet-period",
		Seq:         9999,
		CommittedAt: clock,
	}
	revived.Fingerprint[0] = 0xab
	if err := s.Recover(revived); err != nil {
		t.Fatalf("remembering a record after a full drain released the array: %v", err)
	}
	assertOrderInvariants(t, s, "after reviving a fully-drained store")
	sc, err := NewAgentScope("bus1.agent-0", OpSend, "after-the-quiet-period")
	if err != nil {
		t.Fatalf("building the revived scope: %v", err)
	}
	if _, outcome := s.Lookup(sc, revived.Fingerprint); outcome != OutcomeRetry {
		t.Fatalf("a record remembered after a full drain looks up as %v, want OutcomeRetry: the revived store is not remembering what it accepted, which is a double-apply waiting to happen (invariant 10)", outcome)
	}
}

// TestOrderInvariantsHoldWhenEvictionAndInsertionInterleave is the steady state
// of a busy bus — never fully drained, always both admitting and evicting — and
// it is the case where a dead prefix persists across many operations rather than
// being cleared by a final compaction.
func TestOrderInvariantsHoldWhenEvictionAndInsertionInterleave(t *testing.T) {
	window := time.Hour
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := base
	s := NewStore(StoreOptions{
		Window:     window,
		MaxEntries: 4096,
		Now:        func() time.Time { return clock },
	})

	// Prime with 200 records one millisecond apart.
	for i := 0; i < 200; i++ {
		rec := Record{
			Agent:       fmt.Sprintf("bus1.agent-%d", i%8),
			Op:          OpSend,
			Key:         fmt.Sprintf("k-%06d", i),
			Seq:         uint64(i + 1),
			CommittedAt: base.Add(time.Duration(i) * time.Millisecond),
		}
		rec.Fingerprint[0] = byte(i)
		if err := s.Recover(rec); err != nil {
			t.Fatalf("priming at i=%d: %v", i, err)
		}
	}

	// Now roll forward: each step ages one record out and admits one new one,
	// which is what a bus at steady state does.
	clock = clock.Add(window)
	for i := 200; i < 2000; i++ {
		clock = clock.Add(time.Millisecond)
		rec := Record{
			Agent:       fmt.Sprintf("bus1.agent-%d", i%8),
			Op:          OpSend,
			Key:         fmt.Sprintf("k-%06d", i),
			Seq:         uint64(i + 1),
			CommittedAt: clock,
		}
		rec.Fingerprint[0] = byte(i)
		if err := s.Recover(rec); err != nil {
			t.Fatalf("steady-state insert at i=%d: %v", i, err)
		}
		assertOrderInvariants(t, s, fmt.Sprintf("at steady-state step %d", i))
	}

	s.mu.Lock()
	total := len(s.order)
	retained := len(s.records)
	s.mu.Unlock()
	// The capacity bound in numbers: order's length stays under twice what is
	// retained, however long the bus runs.
	if total >= 2*retained+1 {
		t.Fatalf("after 1800 steady-state operations order holds %d slots for %d retained records, want < %d: the dead prefix is accumulating instead of being compacted", total, retained, 2*retained+1)
	}
}
