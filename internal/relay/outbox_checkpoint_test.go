package relay

// Deeper WAL checkpoint publication/fallback and lifecycle-quota adversarial
// coverage for the Outbox checkpoint participant (Spec Server project
// agent-bus, task 617ffe5a-db42-4aeb-89bb-d9b0889f6c19, v7). This file owns
// ONLY itself: outbox.go, outbox_test.go, and every other file in this
// package are read here but never edited.
//
// TestOutboxCheckpointedTombstoneBoundAcceptance in outbox_test.go is the
// task-level aggregate and is deliberately NOT duplicated here: every test
// below targets an interleaving, boundary or accounting property that
// aggregate does not already pin, using the same ob*/obCheckpointDurable
// fixtures it and this file both rely on (same package, no re-declaration).

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// TestOutboxCheckpointCleanupIgnoresASettlementThatRacesPublication is the
// PRIORITY adversarial test for this task: Checkpoint's cleanup loop
// via `candidate.omittedBodies` (map[id]body captured at Snapshot time),
// re-validating THREE things before it deletes anything: the id is still
// present, still marked `ob.expired`, and its CURRENT canonical encoding
// still equals the exact bytes the candidate captured. A naive
// by-id-only cleanup (`for id := range candidate.omitted { ob.del(id) }`,
// with no re-check) would be defeated by exactly this race: a pending job
// marked expired for omission, then legitimately SETTLED while WAL
// publication is still in flight, changes both its body and its
// `ob.expired` membership (upsertLocked's settle branch clears
// `ob.expired[id]` the moment a pending record turns terminal — see put's
// own comment: "a pending record previously marked for checkpoint omission
// must not cause this newer, correctness-critical terminal state to be
// omitted at the same high-water") — but a stale-by-id-only cleanup reading
// only the ORIGINAL candidate would still delete it.
//
// This test proves the guard holds under exactly that race, deterministically:
// mark a job expired, let Snapshot capture it as omitted, then — while WAL
// publication is deliberately blocked (obCheckpointDurable's
// snapshotted/release channels, the same fixture the existing aggregate's
// "publication reclaims only its immutable generation" subtest already
// uses) — legitimately SETTLE that exact job. The settlement is durably
// written (through the fake's embedded obNullDurable.Write, exactly as a
// live Settle call would write through a real *wal.Log while a real
// checkpoint's generation rename is still in flight) and folded into the
// table, clearing ob.expired and changing the canonical body for it. When
// publication is released and Checkpoint's cleanup runs, it must not delete
// that record: doing so would drop an ACKNOWLEDGED, DURABLE terminal record
// from the live serving table for no reason but a stale id/body captured
// before the settlement existed, while a restart (replaying the same
// durable entries) reconstructs it — the exact live/restart accounting
// divergence invariant 5 (crash recovers to a prefix of accepted history;
// live serving state must not diverge from it either) and invariant 4
// (nothing acknowledged is later un-acknowledged in memory) forbid.
//
// This test is run FIRST and its result reported immediately, per the task's
// explicit instruction to surface a first RED without adapting the assertion
// to hide it. It currently PASSES: the production cleanup loop's
// id+expired+body-equality re-check (outbox.go, Checkpoint) already guards
// against this exact race, and this test pins that as a regression.
func TestOutboxCheckpointCleanupIgnoresASettlementThatRacesPublication(t *testing.T) {
	d := &obCheckpointDurable{snapshotted: make(chan struct{}), release: make(chan struct{})}
	clk := newOBClock()
	ob, err := NewOutbox(OutboxOptions{BusID: obLocalBus, Durable: d, Now: clk.Now,
		RetryHorizon: time.Hour, MaxRetainedJobs: 8, MaxRetainedPerPeer: 8})
	if err != nil {
		t.Fatal(err)
	}

	job, err := ob.Enqueue(obJob(t, 95001))
	if err != nil {
		t.Fatal(err)
	}
	// The enqueue is the only durable entry so far; this is the high-water a
	// real WAL checkpoint would have passed to Snapshot at this exact instant
	// (everything up to and including it is already reflected in the
	// snapshot the checkpoint is about to take).
	enqueueHighWater := uint64(len(d.entries)) + 1

	clk.Advance(time.Hour + time.Second) // past RetryHorizon: eligible for expiry on the next sweep
	d.snapshot = func() ([]byte, error) { return ob.Snapshot(enqueueHighWater) }

	done := make(chan error, 1)
	go func() { done <- ob.Checkpoint() }()
	<-d.snapshotted // Snapshot has run: sweepLocked marked the job expired and
	// the candidate captured it in candidate.omitted. Publication is now
	// blocked on d.release.

	settled, err := ob.Settle(job.JobID, OutboxDelivered, "")
	if err != nil {
		t.Fatalf("Settle raced against blocked checkpoint publication and was refused: %v (a settlement must be unconditional regardless of a job's checkpoint-omission marking)", err)
	}
	if settled.State != OutboxDelivered {
		t.Fatalf("settled state = %s, want %s", settled.State, OutboxDelivered)
	}

	close(d.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// --- THE ASSERTION: the settlement must survive Checkpoint's cleanup ---
	got, ok := ob.Lookup(job.JobID)
	if !ok {
		t.Fatalf("RED: Checkpoint's cleanup deleted job %s by its snapshot-time id alone, discarding a settlement that raced publication and was durably written AFTER the candidate was captured. This is a live acknowledged-state loss: Checkpoint must reclaim only a record that is STILL the exact one its candidate captured (still expired, still the pending record it was), never merely 'this id was in the omitted set'", job.JobID)
	}
	if ok && got.State != OutboxDelivered {
		t.Fatalf("RED: job %s survived Checkpoint's cleanup but its state is %s, not %s — the settlement was partially lost", job.JobID, got.State, OutboxDelivered)
	}

	// --- Live/restart parity: replaying the durable history a real WAL
	// generation-bounded restart would actually walk. A real selected
	// generation's replay tail begins strictly AFTER the generation's own
	// high-water (wal/log.go's `c.CommitIndex <= selection.highWater` skip),
	// and the omitted enqueue record is excluded from the published snapshot
	// too (Snapshot never includes an ob.expired-marked job in s.Records) — so
	// a real recovery NEVER sees the pre-checkpoint enqueue at all, in the
	// snapshot or in the tail. Modeling that by calling Apply on every durable
	// entry and trusting Apply's own CommitIndex<=restoredHighWater skip to
	// filter it out tests something adjacent, not the real shape of recovery:
	// it would still pass even if a real generation's tail selection were
	// broken, because the guard exercised is Apply's internal one, not the
	// tail-truncation recovery actually performs. So the replay here supplies
	// ONLY the post-checkpoint tail (commit indices strictly greater than the
	// checkpoint's own high-water) — proving Restore's rebuilt table plus
	// JUST that bounded tail correctly reconstruct the live terminal state,
	// exactly as a real generation-bounded restart would receive it.
	restarted, err := NewOutbox(OutboxOptions{BusID: obLocalBus, Durable: &obNullDurable{}, Now: clk.Now,
		RetryHorizon: time.Hour, MaxRetainedJobs: 8, MaxRetainedPerPeer: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.Restore(d.published, enqueueHighWater); err != nil {
		t.Fatal(err)
	}
	tailApplied := 0
	for i, e := range d.entries {
		idx := uint64(i + 1)
		commitIndex := idx + 1
		if commitIndex <= enqueueHighWater {
			continue // below the checkpoint's high-water: a real tail never includes this
		}
		if err = restarted.Apply(wal.Committed{PrepareIndex: idx, CommitIndex: commitIndex, Entry: e}); err != nil {
			t.Fatal(err)
		}
		tailApplied++
	}
	if tailApplied != 1 {
		t.Fatalf("post-checkpoint tail replayed %d entries, want exactly 1 (only the settlement; the enqueue is bounded away by the checkpoint's high-water)", tailApplied)
	}
	restartedGot, restartedOK := restarted.Lookup(job.JobID)
	if restartedOK != ok || (restartedOK && ok && restartedGot.State != got.State) {
		t.Fatalf("live/restart parity violated: live={present:%v state:%v} restart={present:%v state:%v} for job %s",
			ok, got.State, restartedOK, restartedGot.State, job.JobID)
	}
}

// TestOutboxCheckpointRestoreAdmitsLegacyRetainedOverageAsDebtOnly is the
// checkpoint-participant-specific analogue of the WAL-replay legacy-debt
// tests already in outbox_test.go (TestOutboxCapacityIsEnforcedOnTheReplayPath
// and friends) — those exercise raw wal.Committed replay directly into
// upsertLocked; this exercises the CHECKPOINT SNAPSHOT/RESTORE surface
// (Snapshot/Restore, the exact pair this task adds), which is a different
// code path (Restore rebuilds the table from a checkpoint's serialized
// participant snapshot rather than folding individual WAL entries one at a
// time) and is not exercised by those tests at all.
//
// A checkpoint taken under generous limits, then restored into an outbox
// configured with TIGHTER limits (as an operator would after a downgrade or
// a deliberate cap tightening), must: admit every pre-existing retained
// record without eviction (never discard a correctness-critical terminal
// state merely to satisfy a cap); log the resulting debt loudly; and refuse
// only NEW growth until a later checkpoint drains it back under the new
// limits.
func TestOutboxCheckpointRestoreAdmitsLegacyRetainedOverageAsDebtOnly(t *testing.T) {
	sink := &obLogSink{}
	d := &obCheckpointDurable{}
	clk := newOBClock()
	generous, err := NewOutbox(OutboxOptions{BusID: obLocalBus, Durable: d, Now: clk.Now,
		Logger: logging.New(sink, logging.LevelDebug), MaxRetainedJobs: 4, MaxRetainedPerPeer: 4})
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(96001); seq < 96004; seq++ {
		j, err := generous.Enqueue(obJob(t, seq))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = generous.Settle(j.JobID, OutboxDelivered, ""); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := generous.Snapshot(999)
	if err != nil {
		t.Fatal(err)
	}

	tightSink := &obLogSink{}
	tight, err := NewOutbox(OutboxOptions{BusID: obLocalBus, Durable: &obNullDurable{}, Now: clk.Now,
		Logger: logging.New(tightSink, logging.LevelDebug), MaxRetainedJobs: 1, MaxRetainedPerPeer: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = tight.Restore(snapshot, 999); err != nil {
		t.Fatalf("Restore refused legacy overage instead of admitting it as debt: %v", err)
	}
	if got := tight.Len(); got != 3 {
		t.Fatalf("Restore evicted pre-existing retained records to satisfy the new, tighter cap: Len()=%d, want 3 (nothing discarded)", got)
	}
	tightSink.mustContain(t, "restore debt", "outbox recovered retained-capacity debt")

	if _, err = tight.Enqueue(obJob(t, 96999)); !errors.Is(err, ErrOutboxCapacity) {
		t.Fatalf("new growth admitted over the tightened retained cap: err=%v, want ErrOutboxCapacity", err)
	}
}

// obCheckpointOpen opens a REAL wal.Log wired as a wal.MultiApplier
// checkpoint participant (rather than obOpen's plain Applier), so lg.Checkpoint
// exercises the actual on-disk generation mechanism this task adds Outbox
// support for. obOpen (outbox_test.go) intentionally never does this: every
// one of its callers only needs replay-on-open, not a real checkpoint.
func obCheckpointOpen(t *testing.T, dir string, tune func(*OutboxOptions)) (*Outbox, *wal.Log) {
	t.Helper()
	o := OutboxOptions{BusID: obLocalBus}
	if tune != nil {
		tune(&o)
	}
	ob, err := NewOutbox(o)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	participants, err := wal.NewMultiApplier(ob)
	if err != nil {
		t.Fatalf("wal.NewMultiApplier: %v", err)
	}
	lg, err := wal.Open(wal.LogOptions{Dir: dir, Checkpoints: participants})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	ob.durable = lg
	return ob, lg
}

// obApplyCounter wraps an *Outbox as a wal.CheckpointParticipant, counting
// every Apply call so a test can prove HOW MANY committed entries a replay
// actually walked — the bound obReplayCommitted cannot see, because it reads
// the raw base WAL file directly and knows nothing about generations or the
// highWater a selected generation bounds replay to (see wal/log.go, the
// `selection.generation != 0 && c.CommitIndex <= selection.highWater` skip in
// the real replay-into-Open path).
type obApplyCounter struct {
	*Outbox
	applied int
}

func (c *obApplyCounter) Apply(committed wal.Committed) error {
	c.applied++
	return c.Outbox.Apply(committed)
}

// TestOutboxCheckpointBoundsThePostCheckpointReplayTail wires the Outbox as a
// real *wal.Log checkpoint participant (obCheckpointOpen) rather than the
// synthetic obCheckpointDurable fake, so this is an END-TO-END proof that the
// Outbox checkpoint participant contract actually bounds what a restart
// replays — not merely that Snapshot and Restore agree on serialized bytes in
// isolation. It checkpoints once, writes more entries afterward, closes, and
// reopens the SAME on-disk log through a counting wrapper: the reopen must
// Apply ONLY the entries written after the checkpoint (a bounded tail), not
// re-walk the pre-checkpoint history the checkpoint already published — while
// still ending up with the exact same live state as the pre-close outbox,
// including the pre-checkpoint settlement the generation's own snapshot must
// carry forward.
func TestOutboxCheckpointBoundsThePostCheckpointReplayTail(t *testing.T) {
	dir := t.TempDir()
	ob, lg := obCheckpointOpen(t, dir, func(o *OutboxOptions) {
		o.RetryHorizon = 2 * time.Hour
		o.MaxRetainedJobs = 8
		o.MaxRetainedPerPeer = 8
	})
	before, err := ob.Enqueue(obJob(t, 97001))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ob.Settle(before.JobID, OutboxDelivered, ""); err != nil {
		t.Fatal(err)
	}
	if err = lg.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	after, err := ob.Enqueue(obJob(t, 97002))
	if err != nil {
		t.Fatal(err)
	}
	if err = lg.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewOutbox(OutboxOptions{BusID: obLocalBus, RetryHorizon: 2 * time.Hour,
		MaxRetainedJobs: 8, MaxRetainedPerPeer: 8})
	if err != nil {
		t.Fatal(err)
	}
	counter := &obApplyCounter{Outbox: reopened}
	participants, err := wal.NewMultiApplier(counter)
	if err != nil {
		t.Fatal(err)
	}
	lg2, err := wal.Open(wal.LogOptions{Dir: dir, Checkpoints: participants})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer lg2.Close()

	if counter.applied != 1 {
		t.Fatalf("reopen Applied %d committed entries, want exactly 1 (only the post-checkpoint enqueue): the checkpointed generation must bound replay away from the pre-checkpoint enqueue+settle, not merely reconstruct the same end state by re-walking them", counter.applied)
	}
	beforeGot, ok := reopened.Lookup(before.JobID)
	if !ok || beforeGot.State != OutboxDelivered {
		t.Fatalf("checkpoint-restored generation lost the pre-checkpoint settlement: present=%v state=%v", ok, beforeGot.State)
	}
	afterGot, ok := reopened.Lookup(after.JobID)
	if !ok || afterGot.State != OutboxPending {
		t.Fatalf("bounded post-checkpoint tail was not replayed correctly: present=%v state=%v", ok, afterGot.State)
	}
	if reopened.Len() != ob.Len() {
		t.Fatalf("reopened Len()=%d, want it to match the live outbox's Len()=%d", reopened.Len(), ob.Len())
	}
}

// TestOutboxSettleIsUnconditionalWhileAtRetainedCapacityDuringPublication
// combines two of the task's required properties that the aggregate does
// not combine: Settle must be unconditional AT the retained cap (never
// refused, never evicting anything), even while a checkpoint publication
// that could otherwise change capacity accounting is concurrently blocked in
// flight.
func TestOutboxSettleIsUnconditionalWhileAtRetainedCapacityDuringPublication(t *testing.T) {
	d := &obCheckpointDurable{snapshotted: make(chan struct{}), release: make(chan struct{})}
	clk := newOBClock()
	ob, err := NewOutbox(OutboxOptions{BusID: obLocalBus, Durable: d, Now: clk.Now,
		MaxJobs: 2, MaxPendingPerPeer: 2, MaxRetainedJobs: 2, MaxRetainedPerPeer: 2,
		MaxRetainedBytes: MaxOutboxRetainedBytes})
	if err != nil {
		t.Fatal(err)
	}
	first, err := ob.Enqueue(obJob(t, 98001))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ob.Enqueue(obJob(t, 98002))
	if err != nil {
		t.Fatal(err)
	}
	// The table is now at MaxRetainedJobs (2 pending records, no tombstones
	// yet): the exact boundary condition.
	if got := ob.Len(); got != 2 {
		t.Fatalf("Len() = %d before capacity boundary setup, want 2", got)
	}

	d.snapshot = func() ([]byte, error) { return ob.Snapshot(2) }
	done := make(chan error, 1)
	go func() { done <- ob.Checkpoint() }()
	<-d.snapshotted

	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, results[0] = ob.Settle(first.JobID, OutboxDelivered, "")
	}()
	go func() {
		defer wg.Done()
		_, results[1] = ob.Settle(second.JobID, OutboxAbandoned, "capacity-boundary settle")
	}()
	wg.Wait()
	close(d.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	for i, err := range results {
		if err != nil {
			t.Fatalf("Settle #%d at retained capacity, racing publication, was refused: %v (a settlement is never refused or evicted for capacity)", i, err)
		}
	}
	got1, ok1 := ob.Lookup(first.JobID)
	got2, ok2 := ob.Lookup(second.JobID)
	if !ok1 || got1.State != OutboxDelivered {
		t.Fatalf("first job lost its settlement: present=%v state=%v", ok1, got1.State)
	}
	if !ok2 || got2.State != OutboxAbandoned {
		t.Fatalf("second job lost its settlement: present=%v state=%v", ok2, got2.State)
	}
}
