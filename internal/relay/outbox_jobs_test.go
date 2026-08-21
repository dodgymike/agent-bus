package relay

import (
	"reflect"
	"testing"
	"time"
)

// RELAY-54's acceptance evidence for the QUERY half: Outbox.Jobs.
//
// Pending() answered only "what does this bus still owe?". An ABANDONED job — a
// message this bus accepted and will never deliver, which invariant 6 requires
// be recorded specifically rather than discarded silently — was written durably
// and reachable from NO caller. Jobs(OutboxAbandoned) is the case that did not
// exist before this task, and it is the reason the task was filed; every other
// case here exists to pin that widening the selector did not loosen it.
//
// Every helper is prefixed obJobs* so it cannot collide with the ob* fixtures
// outbox_test.go owns.

const (
	// Three DISTINCT peers, all distinct from obLocalBus: upsertLocked refuses a
	// record whose PeerBusID is this bus's own id, and per-peer rows are easier
	// to read when one peer holds exactly one state.
	obJobsPeerA = "bus-jobs-peer-a"
	obJobsPeerB = "bus-jobs-peer-b"
	obJobsPeerC = "bus-jobs-peer-c"
	obJobsPeerD = "bus-jobs-peer-d"
)

// obJobsEnqueue durably records one job for peer and returns it.
func obJobsEnqueue(t *testing.T, ob *Outbox, peer string, seq uint64) OutboxRecord {
	t.Helper()
	rec, err := ob.Enqueue(OutboxJob{
		PeerBusID:       peer,
		OriginMessageID: obMessageID(t, seq),
		Size:            11,
		ContentSHA256:   obHash,
	})
	if err != nil {
		t.Fatalf("Enqueue(peer %s, seq %d): %v", peer, seq, err)
	}
	return rec
}

// obJobsSettle moves a job to a terminal state.
func obJobsSettle(t *testing.T, ob *Outbox, jobID string, state OutboxState, reason string) OutboxRecord {
	t.Helper()
	rec, err := ob.Settle(jobID, state, reason)
	if err != nil {
		t.Fatalf("Settle(%s, %s): %v", jobID, state, err)
	}
	return rec
}

// obJobsIDs is a record slice as a comparable list of job ids, IN THE ORDER
// RETURNED — the order is part of what is under test, so this must not sort.
func obJobsIDs(recs []OutboxRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.JobID)
	}
	return out
}

// obJobsFixture builds the three-state table every selection case is asserted
// against: ONE pending job, ONE delivered job and ONE abandoned job, each to a
// different peer, all enqueued at the SAME instant so the tie-break is what
// orders them (peer a < peer b < peer c, and a job id begins with its peer).
func obJobsFixture(t *testing.T) (ob *Outbox, clk *obClock, pending, delivered, abandoned string) {
	t.Helper()
	ob, _, clk, _ = obNewOutbox(t, nil)

	p := obJobsEnqueue(t, ob, obJobsPeerA, 1)
	d := obJobsEnqueue(t, ob, obJobsPeerB, 2)
	a := obJobsEnqueue(t, ob, obJobsPeerC, 3)

	obJobsSettle(t, ob, d.JobID, OutboxDelivered, "")
	obJobsSettle(t, ob, a.JobID, OutboxAbandoned, "the peer answered 410 and will not take it")

	return ob, clk, p.JobID, d.JobID, a.JobID
}

// ---------------------------------------------------------------------------
// The selector
// ---------------------------------------------------------------------------

// TestOutboxJobsSelectsExactlyTheRequestedStates is the table this task exists
// for.
//
// The load-bearing row is `only abandoned`: before RELAY-54 there was NO caller
// that could reach an abandoned record, so a bus that had given up on a message
// looked identical, from every query this package exported, to a bus that never
// had one.
func TestOutboxJobsSelectsExactlyTheRequestedStates(t *testing.T) {
	t.Parallel()
	ob, _, pending, delivered, abandoned := obJobsFixture(t)

	cases := []struct {
		name   string
		states []OutboxState
		want   []string
	}{
		{
			// No states at all is EVERY state, which is the convention the CLI's
			// empty -state filter is written against.
			name:   "no states returns every state",
			states: nil,
			want:   []string{pending, delivered, abandoned},
		},
		{
			name:   "only pending",
			states: []OutboxState{OutboxPending},
			want:   []string{pending},
		},
		{
			name:   "only delivered",
			states: []OutboxState{OutboxDelivered},
			want:   []string{delivered},
		},
		{
			// THE CASE THAT DID NOT EXIST BEFORE RELAY-54.
			name:   "only abandoned",
			states: []OutboxState{OutboxAbandoned},
			want:   []string{abandoned},
		},
		{
			// The operator question in full: what is stuck, and what is lost —
			// and NOT what was delivered fine.
			name:   "pending and abandoned, never the delivered one",
			states: []OutboxState{OutboxPending, OutboxAbandoned},
			want:   []string{pending, abandoned},
		},
		{
			// Membership is tested PER RECORD rather than by iterating the
			// filter, so a filter assembled from repeatable CLI flags does not
			// have to be deduplicated first. A record appearing twice would be
			// double-counted by every caller that counts.
			name:   "a duplicated state yields the record once",
			states: []OutboxState{OutboxPending, OutboxPending},
			want:   []string{pending},
		},
		{
			name:   "every state named explicitly matches naming none",
			states: []OutboxState{OutboxPending, OutboxDelivered, OutboxAbandoned},
			want:   []string{pending, delivered, abandoned},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := obJobsIDs(ob.Jobs(tc.states...))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Jobs(%v) = %v, want %v", tc.states, got, tc.want)
			}
		})
	}
}

// TestOutboxJobsPendingIsExactlyJobsPending pins the refactor's own claim:
// Pending() is now Jobs(OutboxPending) and nothing else, so the pending set and
// the operator view can never disagree about which records the sweep has
// already taken out of the answer.
//
// The comparison is on the WHOLE records, not on their ids: a second
// implementation that returned the right jobs with different fields would be
// the drift this consolidation exists to make impossible.
func TestOutboxJobsPendingIsExactlyJobsPending(t *testing.T) {
	t.Parallel()
	ob, _, pending, _, _ := obJobsFixture(t)
	// A second pending job, so "identical" is not trivially true of a one-record
	// answer.
	second := obJobsEnqueue(t, ob, obJobsPeerD, 4)

	viaPending := ob.Pending()
	viaJobs := ob.Jobs(OutboxPending)
	if !reflect.DeepEqual(viaPending, viaJobs) {
		t.Fatalf("Pending() and Jobs(OutboxPending) disagree:\n  Pending(): %v\n  Jobs():    %v",
			obJobsIDs(viaPending), obJobsIDs(viaJobs))
	}
	if got, want := obJobsIDs(viaPending), []string{pending, second.JobID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Pending() = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// The order
// ---------------------------------------------------------------------------

// TestOutboxJobsOrderIsOldestEnqueuedThenJobID asserts the documented order with
// records DELIBERATELY SHARING an EnqueuedAt, which is the only shape in which
// the tie-break is observable at all.
//
// Without the tie-break the answer would be Go's map order, which is randomised
// per run: a restart would re-offer jobs in a different sequence than it would
// have sent them, and an operator diffing two runs of `agent-bus outbox` would
// see a different report each time from an unchanged directory.
func TestOutboxJobsOrderIsOldestEnqueuedThenJobID(t *testing.T) {
	t.Parallel()
	ob, _, clk, _ := obNewOutbox(t, nil)

	// Enqueued in DESCENDING peer order at ONE instant, so insertion order is
	// the reverse of the expected answer and cannot be what produces it.
	c := obJobsEnqueue(t, ob, obJobsPeerC, 3)
	a := obJobsEnqueue(t, ob, obJobsPeerA, 1)
	b := obJobsEnqueue(t, ob, obJobsPeerB, 2)
	if !a.EnqueuedAt.Equal(b.EnqueuedAt) || !b.EnqueuedAt.Equal(c.EnqueuedAt) {
		t.Fatalf("the fixture did not produce a shared EnqueuedAt, so the tie-break is not exercised: %s / %s / %s",
			a.EnqueuedAt, b.EnqueuedAt, c.EnqueuedAt)
	}

	// One strictly LATER record, which must sort last however its id compares:
	// peer a's id sorts FIRST, so a stable-but-unsorted implementation, or one
	// that sorted on the id alone, puts it in the wrong place.
	clk.Advance(time.Hour)
	later := obJobsEnqueue(t, ob, obJobsPeerA, 9)
	if !later.EnqueuedAt.After(a.EnqueuedAt) {
		t.Fatalf("the fixture's later record is not later: %s vs %s", later.EnqueuedAt, a.EnqueuedAt)
	}

	want := []string{a.JobID, b.JobID, c.JobID, later.JobID}
	if got := obJobsIDs(ob.Jobs()); !reflect.DeepEqual(got, want) {
		t.Fatalf("Jobs() order = %v, want oldest-EnqueuedAt first with the job id as the tie-break: %v", got, want)
	}
	// The same order must hold through a filter, since the CLI slices this list
	// in place and relies on the selection preserving it.
	if got := obJobsIDs(ob.Jobs(OutboxPending)); !reflect.DeepEqual(got, want) {
		t.Fatalf("Jobs(OutboxPending) order = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// The retention window
// ---------------------------------------------------------------------------

// TestOutboxJobsExcludesASweptRecord: every exported entry point sweeps first,
// so a job past the retry horizon and a tombstone past its retention window are
// GONE from the answer.
//
// This is the limit any caller rendering this for a human must state, and it is
// asserted here rather than assumed: the record is present before the clock
// moves and absent after, so the exclusion is demonstrably the sweep and not a
// fixture that never held the record.
func TestOutboxJobsExcludesASweptRecord(t *testing.T) {
	t.Parallel()
	ob, clk, pending, delivered, abandoned := obJobsFixture(t)

	if got, want := obJobsIDs(ob.Jobs()), []string{pending, delivered, abandoned}; !reflect.DeepEqual(got, want) {
		t.Fatalf("before the clock moves, Jobs() = %v, want %v", got, want)
	}

	// Past BOTH windows: OutboxRetryHorizon for the pending job, and
	// OutboxSettledRetention for the two tombstones. Bound by reference rather
	// than as a literal 24h, so a change to either constant moves this test with
	// it instead of leaving it asserting about a window nothing uses.
	horizon := OutboxRetryHorizon
	if OutboxSettledRetention > horizon {
		horizon = OutboxSettledRetention
	}
	clk.Advance(horizon + time.Minute)

	if got := ob.Jobs(); len(got) != 0 {
		t.Fatalf("Jobs() still reports %v after the retention window passed; a swept record must not be in the answer", obJobsIDs(got))
	}
	for _, states := range [][]OutboxState{
		{OutboxPending},
		{OutboxDelivered},
		{OutboxAbandoned},
	} {
		if got := ob.Jobs(states...); len(got) != 0 {
			t.Fatalf("Jobs(%v) still reports %v after the retention window passed", states, obJobsIDs(got))
		}
	}
	if got := ob.Pending(); len(got) != 0 {
		t.Fatalf("Pending() still reports %v after the retention window passed", obJobsIDs(got))
	}
}
