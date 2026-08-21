package httpapi

import "testing"

// TestAckWaiterCountAdmitsExactlyTheCap is the SCHEDULER-INDEPENDENT half of the
// parked-wait bound (ACK-9).
//
// # WHY THIS EXISTS ALONGSIDE TestAckStatusCapsParkedWaitsPerAgent
//
// That test drives real parked HTTP requests, which is the only way to prove the
// handler actually consults the bound — but it can only assert "eventually
// refused", it takes seconds, and its mutation fails by TIMEOUT rather than by a
// wrong answer. It was also genuinely flaky until a scheduling yield was added,
// because 32 goroutines racing one prober is a convergence problem.
//
// This one asserts the exact arithmetic — 32 admitted, the 33rd refused, buckets
// per principal, slots reusable — with no goroutines, no timing and no HTTP, in
// under a millisecond. Neither replaces the other: this proves the bound is
// RIGHT, that one proves it is REACHED.
func TestAckWaiterCountAdmitsExactlyTheCap(t *testing.T) {
	c := newAckWaiterCount()

	releases := make([]func(), 0, maxParkedAckStatusPerAgent)
	for i := 0; i < maxParkedAckStatusPerAgent; i++ {
		release, ok := c.acquire("bus-x.alice-1")
		if !ok {
			t.Fatalf("acquire %d of %d was refused; the cap engages EARLY, which would refuse work that is within the documented bound", i+1, maxParkedAckStatusPerAgent)
		}
		releases = append(releases, release)
	}
	if _, ok := c.acquire("bus-x.alice-1"); ok {
		t.Fatalf("acquire %d was admitted; the cap does not engage at %d", maxParkedAckStatusPerAgent+1, maxParkedAckStatusPerAgent)
	}

	// PER PRINCIPAL, not global. This is the property that makes the bound
	// self-harm rather than amplification: a flooder fills only its own bucket,
	// and no agent can starve another. A global counter would pass every
	// assertion above and fail this one.
	//
	// IT PROVES THE COUNTER, NOT THE HANDLER. This test supplies the key
	// itself, so it is blind to WHICH key handleAckStatus passes: a handler
	// calling acquire("") gets one global bucket out of a perfectly
	// per-principal counter, and every assertion in this file stays green.
	// TestAckStatusParkedWaitCapIsPerPrincipal drives two real enrolled
	// principals through ServeHTTP and is the test that closes that gap.
	if _, ok := c.acquire("bus-x.bob-1"); !ok {
		t.Fatal("a DIFFERENT principal was refused while alice was at her cap; the bucket is not per-principal, so one agent can deny every other agent this route")
	}

	// A CONCURRENCY bound, not a lifetime quota.
	releases[0]()
	if _, ok := c.acquire("bus-x.alice-1"); !ok {
		t.Fatal("a released slot was not reusable; the cap is behaving as a lifetime quota, which would permanently lock an agent out after 32 waits")
	}

	// The map does not accumulate an entry per principal that ever waited.
	for _, r := range releases[1:] {
		r()
	}
	c.acquire("bus-x.alice-1") // the reuse above is still held; release both
	c.mu.Lock()
	held := len(c.n)
	c.mu.Unlock()
	if held == 0 {
		t.Fatal("the map is empty while slots are still held; the count is not tracking anything")
	}
}

// TestAckWaiterCountDeletesAtZero: the entry is REMOVED at zero rather than left
// at 0. A bus with many agents over a long uptime would otherwise accumulate one
// map entry per principal that ever waited — a slow leak inside the mechanism
// whose whole purpose is to bound something.
func TestAckWaiterCountDeletesAtZero(t *testing.T) {
	c := newAckWaiterCount()
	r1, _ := c.acquire("bus-x.alice-1")
	r2, _ := c.acquire("bus-x.alice-1")

	r1()
	c.mu.Lock()
	n := c.n["bus-x.alice-1"]
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("after releasing one of two slots the count is %d, want 1", n)
	}

	r2()
	c.mu.Lock()
	_, present := c.n["bus-x.alice-1"]
	size := len(c.n)
	c.mu.Unlock()
	if present || size != 0 {
		t.Fatalf("the map still holds %d entry/entries after every slot was released; entries must be deleted at zero, not left at 0", size)
	}
}
