package ack

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/wal"
)

// fakeLog is a durable log that remembers what was written.
//
// It records the CANONICAL BYTES, which is what makes replayFrom below a real
// test of recovery rather than a copy of the in-memory table: the rebuilt store
// only ever sees what would have been on disk.
type fakeLog struct {
	mu sync.Mutex
	// applyTo, when set, is fed every committed entry synchronously — exactly
	// as wal.Log.Write feeds the registered applier. It is how the live-write
	// double-fold is exercised.
	applyTo *Store
	entries []wal.Entry
	index   uint64
	err     error
}

func (f *fakeLog) Write(e wal.Entry) (wal.Committed, error) {
	f.mu.Lock()
	if f.err != nil {
		err := f.err
		f.mu.Unlock()
		return wal.Committed{}, err
	}
	f.index += 2
	c := wal.Committed{Entry: e, PrepareIndex: f.index - 1, CommitIndex: f.index}
	f.entries = append(f.entries, e)
	applyTo := f.applyTo
	f.mu.Unlock()
	if applyTo != nil {
		if err := applyTo.Apply(c); err != nil {
			return wal.Committed{}, err
		}
	}
	return c, nil
}

func (f *fakeLog) written() []wal.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]wal.Entry, len(f.entries))
	copy(out, f.entries)
	return out
}

// replayFrom rebuilds a table from the recorded bytes ALONE, in commit order —
// what wal.Open does at startup.
func (f *fakeLog) replayFrom(t *testing.T, o Options) *Store {
	t.Helper()
	s := NewStore(o)
	for i, e := range f.written() {
		if err := s.Apply(wal.Committed{Entry: e, PrepareIndex: uint64(2*i + 1), CommitIndex: uint64(2*i + 2)}); err != nil {
			t.Fatalf("replaying entry %d: %v", i, err)
		}
	}
	return s
}

// newTestStore returns an attached store and its log, on a controllable clock.
func newTestStore(t *testing.T, o Options) (*Store, *fakeLog) {
	t.Helper()
	lg := &fakeLog{}
	s := NewStore(o)
	if err := s.Attach(lg); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return s, lg
}

func mustAccept(t *testing.T, s *Store, recipient string) {
	t.Helper()
	if err := s.Accept(testKey, testSender, recipient); err != nil {
		t.Fatalf("Accept(%s): %v", recipient, err)
	}
}

// TestUnattachedRefusesEveryWrite: a table with no log is a REFUSAL, not a
// degraded in-memory mode. One that answered from memory would report delivery
// outcomes no restart could reproduce.
func TestUnattachedRefusesEveryWrite(t *testing.T) {
	s := NewStore(Options{})
	if err := s.Accept(testKey, testSender, testRecipient); !errors.Is(err, ErrNotDurable) {
		t.Errorf("Accept on an unattached table = %v, want ErrNotDurable", err)
	}
	if err := s.MarkInFlight(testKey, testRecipient); !errors.Is(err, ErrNotDurable) {
		t.Errorf("MarkInFlight on an unattached table = %v, want ErrNotDurable", err)
	}
	if err := s.Settle(testKey, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); !errors.Is(err, ErrNotDurable) {
		t.Errorf("Settle on an unattached table = %v, want ErrNotDurable", err)
	}
	if err := s.Attach(nil); err == nil {
		t.Error("Attach(nil) succeeded")
	}
	lg := &fakeLog{}
	if err := s.Attach(lg); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := s.Attach(lg); err == nil {
		t.Error("a second Attach succeeded; one in-memory table must not have two durable histories")
	}
}

// TestAcceptIsDurableAndIdempotent covers invariant 4 (the row is written before
// the call returns) and invariant 10's FIRST case (a repeat is a legitimate
// retry: original result, nothing re-applied, no error).
func TestAcceptIsDurableAndIdempotent(t *testing.T) {
	s, lg := newTestStore(t, Options{})
	mustAccept(t, s, testRecipient)

	if n := len(lg.written()); n != 1 {
		t.Fatalf("Accept wrote %d durable entries, want 1", n)
	}
	if k := lg.written()[0].Kind; k != RecordKind {
		t.Fatalf("Accept wrote kind %q, want %q", k, RecordKind)
	}
	r, ok := s.Lookup(testKey, testRecipient)
	if !ok || r.State != StateAccepted {
		t.Fatalf("Lookup after Accept = (%+v, %v), want an accepted row", r, ok)
	}
	first := r.AcceptedAt

	// The retry. Nothing written, no error, and the ORIGINAL accepted_at — so
	// the retention window fires from the first acceptance and cannot be pushed
	// out by retrying.
	if err := s.Accept(testKey, testSender, testRecipient); err != nil {
		t.Fatalf("a repeat Accept returned %v, want nil: it is a legitimate retry of a lost acknowledgement", err)
	}
	if n := len(lg.written()); n != 1 {
		t.Fatalf("a repeat Accept wrote %d durable entries in total, want 1: a retry must re-apply NOTHING", n)
	}
	again, _ := s.Lookup(testKey, testRecipient)
	if !again.AcceptedAt.Equal(first) {
		t.Errorf("a repeat Accept moved accepted_at from %s to %s; the retention anchor must not be pushed out by retrying", first, again.AcceptedAt)
	}
}

// TestAcceptRetryOnATerminalRowDoesNotReopenIt is the case that would be easy to
// get wrong: a send retried after the recipient already ACKed must not drag a
// terminal row back to `accepted`, and must not tell the caller its send failed.
func TestAcceptRetryOnATerminalRowDoesNotReopenIt(t *testing.T) {
	s, lg := newTestStore(t, Options{})
	mustAccept(t, s, testRecipient)
	if err := s.Settle(testKey, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	before := len(lg.written())

	if err := s.Accept(testKey, testSender, testRecipient); err != nil {
		t.Fatalf("Accept on a delivered row = %v, want nil", err)
	}
	if n := len(lg.written()); n != before {
		t.Fatalf("Accept on a delivered row wrote %d entries (was %d); terminal is ABSORBING", n-before, before)
	}
	r, _ := s.Lookup(testKey, testRecipient)
	if r.State != StateDelivered {
		t.Fatalf("the row is %s after a late Accept, want delivered: a terminal state is never reopened", r.State)
	}
}

// TestStateMachine walks ACK-CONTRACT.md §8.2 as a table. Each row names the
// event sequence and the state it must converge on, plus whether the last event
// is allowed at all.
func TestStateMachine(t *testing.T) {
	type step struct {
		do   func(*Store) error
		name string
	}
	accept := step{name: "E1 accept", do: func(s *Store) error { return s.Accept(testKey, testSender, testRecipient) }}
	inflight := step{name: "E2 in_flight", do: func(s *Store) error { return s.MarkInFlight(testKey, testRecipient) }}
	deliver := step{name: "E5 delivered", do: func(s *Store) error {
		return s.Settle(testKey, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified)
	}}
	refuse := step{name: "E6 refused", do: func(s *Store) error {
		return s.Settle(testKey, testRecipient, StateRefused, ClassRecipientRefusedPolicy, AttestedByRecipientSignatureUnverified)
	}}
	undeliverable := step{name: "E4 undeliverable", do: func(s *Store) error {
		return s.Settle(testKey, testRecipient, StateUndeliverable, ClassHorizonExpired, AttestedByPeerBus)
	}}

	cases := []struct {
		name    string
		steps   []step
		wantErr error
		want    State
		absent  bool
	}{
		{name: "no record: in_flight is rejected", steps: []step{inflight}, wantErr: ErrNoRecord, absent: true},
		{name: "no record: delivered is rejected", steps: []step{deliver}, wantErr: ErrNoRecord, absent: true},
		{name: "no record: refused is rejected", steps: []step{refuse}, wantErr: ErrNoRecord, absent: true},
		{name: "accepted", steps: []step{accept}, want: StateAccepted},
		{name: "accepted -> in_flight", steps: []step{accept, inflight}, want: StateInFlight},
		{name: "accepted -> in_flight -> in_flight is idempotent", steps: []step{accept, inflight, inflight}, want: StateInFlight},
		{name: "accepted -> delivered (the local-recipient path)", steps: []step{accept, deliver}, want: StateDelivered},
		{name: "accepted -> refused", steps: []step{accept, refuse}, want: StateRefused},
		{name: "in_flight -> undeliverable", steps: []step{accept, inflight, undeliverable}, want: StateUndeliverable},
		{name: "delivered -> delivered is a legitimate retry", steps: []step{accept, deliver, deliver}, want: StateDelivered},
		{name: "delivered -> refused is a VIOLATION", steps: []step{accept, deliver, refuse}, wantErr: ErrTerminal, want: StateDelivered},
		{name: "refused -> delivered is a VIOLATION", steps: []step{accept, refuse, deliver}, wantErr: ErrTerminal, want: StateRefused},
		{name: "undeliverable -> delivered is a VIOLATION", steps: []step{accept, undeliverable, deliver}, wantErr: ErrTerminal, want: StateUndeliverable},
		{name: "delivered -> in_flight is IGNORED, not an error", steps: []step{accept, deliver, inflight}, want: StateDelivered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(t, Options{})
			var err error
			for i, st := range tc.steps {
				err = st.do(s)
				if i < len(tc.steps)-1 && err != nil {
					t.Fatalf("step %d (%s) failed early: %v", i, st.name, err)
				}
			}
			if tc.wantErr == nil && err != nil {
				t.Fatalf("last step = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("last step = %v, want %v", err, tc.wantErr)
			}
			r, ok := s.Lookup(testKey, testRecipient)
			if tc.absent {
				if ok {
					t.Fatalf("a row exists (%+v); nothing binds an event for a pair with no record", r)
				}
				return
			}
			if !ok {
				t.Fatalf("no row after %d steps, want %s", len(tc.steps), tc.want)
			}
			if r.State != tc.want {
				t.Fatalf("the row is %s, want %s", r.State, tc.want)
			}
		})
	}
}

// TestSettleRefusesANonTerminalTarget: a settle moves a row OUT of the
// non-terminal states. Accepting `accepted` here would give a caller a way to
// rewrite a row's timestamps.
func TestSettleRefusesANonTerminalTarget(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	mustAccept(t, s, testRecipient)
	for _, st := range []State{StateAccepted, StateInFlight, StateInvalid} {
		if err := s.Settle(testKey, testRecipient, st, "", AttestedByPeerBus); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("Settle to %s = %v, want ErrInvalidRecord", st, err)
		}
	}
}

// TestTerminalPreservesAcceptedAt: a terminal row records when the message was
// ACCEPTED, not when it settled. Re-stamping would push the retention anchor out
// every time a row transitioned.
func TestTerminalPreservesAcceptedAt(t *testing.T) {
	now := testAccepted
	s, _ := newTestStore(t, Options{Now: func() time.Time { return now }})
	mustAccept(t, s, testRecipient)
	now = testAccepted.Add(time.Hour)
	if err := s.Settle(testKey, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	r, _ := s.Lookup(testKey, testRecipient)
	if !r.AcceptedAt.Equal(testAccepted) {
		t.Errorf("accepted_at is %s after a settle, want the original %s", r.AcceptedAt, testAccepted)
	}
	if !r.SettledAt.Equal(now) {
		t.Errorf("settled_at is %s, want %s", r.SettledAt, now)
	}
}

// TestPerRecipientRows is §3.2, which is the decision that lets broadcast be
// specified later with no on-disk change: the row is keyed on (correlation key,
// RECIPIENT), so one message to N recipients is N independent rows whose states
// move independently.
func TestPerRecipientRows(t *testing.T) {
	const other = "testbus.gamma-1"
	s, _ := newTestStore(t, Options{})
	mustAccept(t, s, testRecipient)
	mustAccept(t, s, other)
	if n := s.Len(); n != 2 {
		t.Fatalf("the table holds %d rows for one message to two recipients, want 2", n)
	}
	if err := s.Settle(testKey, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	a, _ := s.Lookup(testKey, testRecipient)
	b, _ := s.Lookup(testKey, other)
	if a.State != StateDelivered || b.State != StateAccepted {
		t.Fatalf("one recipient settling moved the other: %s / %s, want delivered / accepted", a.State, b.State)
	}
}

// TestRestartRebuildsFromTheLogAlone is invariant 5 at the unit level: the
// rebuilt table is a function of the durable bytes and nothing else.
func TestRestartRebuildsFromTheLogAlone(t *testing.T) {
	const other = "testbus.gamma-1"
	s, lg := newTestStore(t, Options{})
	mustAccept(t, s, testRecipient)
	mustAccept(t, s, other)
	if err := s.MarkInFlight(testKey, other); err != nil {
		t.Fatalf("MarkInFlight: %v", err)
	}
	if err := s.Settle(testKey, testRecipient, StateRefused, ClassRecipientRefusedUndecodable, AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	rebuilt := lg.replayFrom(t, Options{})
	if n := rebuilt.Len(); n != 2 {
		t.Fatalf("the rebuilt table holds %d rows, want 2", n)
	}
	a, ok := rebuilt.Lookup(testKey, testRecipient)
	if !ok || a.State != StateRefused || a.Class != ClassRecipientRefusedUndecodable {
		t.Fatalf("the rebuilt row is (%+v, %v), want refused/recipient_refused_undecodable", a, ok)
	}
	b, ok := rebuilt.Lookup(testKey, other)
	if !ok || b.State != StateInFlight {
		t.Fatalf("the rebuilt row is (%+v, %v), want in_flight", b, ok)
	}
}

// TestReplayOutOfOrderCannotResurrectATerminalRow is the monotonicity rule.
//
// Replaying a stale `accepted` record AFTER a terminal one must be REFUSED, not
// applied: applying it would tell a sender a delivered message is still in
// flight, which is an inversion of the truth rather than a gap in it.
func TestReplayOutOfOrderCannotResurrectATerminalRow(t *testing.T) {
	s, lg := newTestStore(t, Options{})
	mustAccept(t, s, testRecipient)
	if err := s.Settle(testKey, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	entries := lg.written()
	if len(entries) != 2 {
		t.Fatalf("expected an accept and a settle, got %d entries", len(entries))
	}
	// REVERSED: the terminal record first, the acceptance second.
	rebuilt := NewStore(Options{})
	for i, e := range []wal.Entry{entries[1], entries[0]} {
		if err := rebuilt.Apply(wal.Committed{Entry: e, PrepareIndex: uint64(2*i + 1), CommitIndex: uint64(2*i + 2)}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	r, ok := rebuilt.Lookup(testKey, testRecipient)
	if !ok {
		t.Fatal("no row after a reversed replay")
	}
	if r.State != StateDelivered {
		t.Fatalf("a reversed replay left the row %s, want delivered: terminal is ABSORBING whatever order records arrive in", r.State)
	}
}

// TestApplyNeverErrors: Apply is on the replay path, where returning an error
// for an entry whose COMMIT is already durable poisons the whole log with
// wal.ErrDiverged. A record it cannot use is DISCARDED and logged, never
// escalated.
func TestApplyNeverErrors(t *testing.T) {
	s := NewStore(Options{})
	cases := []wal.Committed{
		{Entry: wal.Entry{Kind: RecordKind, Body: []byte(`not json`)}},
		{Entry: wal.Entry{Kind: RecordKind, Body: []byte(`{"record_version":1,"state":"unknown"}`)}},
		{Entry: wal.Entry{Kind: RecordKind, Body: nil}},
		{Entry: wal.Entry{Kind: "message", Body: []byte(`{"anything":1}`)}},
	}
	for i, c := range cases {
		if err := s.Apply(c); err != nil {
			t.Fatalf("case %d: Apply returned %v; an applier that errors on an already-committed entry poisons the log", i, err)
		}
	}
	if n := s.Len(); n != 0 {
		t.Fatalf("the table holds %d rows after applying only damaged records, want 0", n)
	}
}

// TestLiveWriteAndApplyDoNotDoubleCount exercises the production shape, where
// the multiplex applier hands a LIVE commit to Apply and the mutating method
// then folds the identical canonical record in again. The second fold must be a
// no-op.
func TestLiveWriteAndApplyDoNotDoubleCount(t *testing.T) {
	lg := &fakeLog{}
	s := NewStore(Options{})
	lg.applyTo = s
	if err := s.Attach(lg); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	mustAccept(t, s, testRecipient)
	if err := s.Settle(testKey, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if n := s.Len(); n != 1 {
		t.Fatalf("the table holds %d rows, want 1: Apply and foldIn must converge on one row", n)
	}
	st := s.Stats()
	if st.Senders != 1 {
		t.Fatalf("the table counts %d senders, want 1: the per-sender counter must not be incremented twice for one row", st.Senders)
	}
}

// TestCapacityRefusesAndEvictsNothing is §11.2/§11.3: the hard cap is
// fail-closed and NOTHING is evicted. Evicting a live row turns a real outcome
// into a false `unknown` — an inversion of the truth, not a gap in it.
func TestCapacityRefusesAndEvictsNothing(t *testing.T) {
	// A DISTINCT SENDER PER ROW, deliberately: this test is about the BUS-WIDE
	// cap, and sharing a sender would trip the per-sender fair share first (a
	// two-row table's pressure line is one row). The two bounds are separate
	// rules and are tested separately — see TestPerSenderFairShare.
	s, _ := newTestStore(t, Options{MaxEntries: 2})
	for i := 1; i <= 2; i++ {
		if err := s.Accept(fmt.Sprintf("testbus-%d", i), fmt.Sprintf("testbus.sender%d-1", i), testRecipient); err != nil {
			t.Fatalf("Accept %d: %v", i, err)
		}
	}
	err := s.Accept("testbus-3", "testbus.sender3-1", testRecipient)
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("Accept past the cap = %v, want ErrCapacity", err)
	}
	if !strings.Contains(err.Error(), "THE MESSAGE IS UNAFFECTED") {
		t.Errorf("the refusal does not say the message is unaffected, which is the whole point of the degradation: %v", err)
	}
	if n := s.Len(); n != 2 {
		t.Fatalf("the table holds %d rows after a refusal, want 2: nothing may be evicted to make room", n)
	}
	for i := 1; i <= 2; i++ {
		if _, ok := s.Lookup(fmt.Sprintf("testbus-%d", i), testRecipient); !ok {
			t.Errorf("row %d was evicted to make room for a refused one", i)
		}
	}
	if st := s.Stats(); st.CapacityRefusals == 0 {
		t.Error("Stats reports 0 capacity refusals; the degradation must be OBSERVABLE, not silent")
	}
	// A transition on an EXISTING row must still succeed at capacity: a terminal
	// outcome that cannot be recorded is a real result lost, which is worse than
	// an unobserved one.
	if err := s.Settle("testbus-1", testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("Settle at capacity = %v, want nil: a transition does not grow the table", err)
	}
}

// TestPerSenderFairShare is the rule that stops one authenticated agent denying
// status to every other agent on the bus.
//
// With maxEntries = 8 the pressure line is 4 and the share for a table holding
// one sender is 8/(1+1) = 4. So the greedy sender is cut off at 4 rows while a
// second sender can still get one.
func TestPerSenderFairShare(t *testing.T) {
	const greedy, victim = "testbus.greedy-1", "testbus.victim-1"
	s, _ := newTestStore(t, Options{MaxEntries: 8})
	var lastErr error
	admitted := 0
	for i := 1; i <= 8; i++ {
		if err := s.Accept(fmt.Sprintf("testbus-%d", i), greedy, testRecipient); err != nil {
			lastErr = err
			break
		}
		admitted++
	}
	if !errors.Is(lastErr, ErrAgentQuota) {
		t.Fatalf("the greedy sender was stopped by %v after %d rows, want ErrAgentQuota", lastErr, admitted)
	}
	if admitted >= 8 {
		t.Fatalf("the greedy sender took %d of 8 rows; the fair share must engage above the pressure line", admitted)
	}
	if err := s.Accept("testbus-100", victim, testRecipient); err != nil {
		t.Fatalf("a second sender's FIRST row was refused with %v; the +1 divisor exists precisely to reserve room for the sender that has not arrived yet", err)
	}
}

// TestSweepRetiresRowsAtTheWindow is §11's event E9. A swept row reports as
// `unknown`, which is honest: the window is chosen so nothing can still change
// after it.
func TestSweepRetiresRowsAtTheWindow(t *testing.T) {
	now := testAccepted
	s, _ := newTestStore(t, Options{Now: func() time.Time { return now }})
	mustAccept(t, s, testRecipient)

	now = testAccepted.Add(Retention - time.Second)
	if _, ok := s.Lookup(testKey, testRecipient); !ok {
		t.Fatal("the row was swept before its window ran out")
	}
	now = testAccepted.Add(Retention)
	if _, ok := s.Lookup(testKey, testRecipient); ok {
		t.Fatal("the row outlived its retention window")
	}
	if n := s.Len(); n != 0 {
		t.Fatalf("the table holds %d rows after the sweep, want 0", n)
	}
	// And the per-sender bookkeeping must be swept with it, or a sender's fair
	// share would be consumed for ever by rows that no longer exist.
	if st := s.Stats(); st.Senders != 0 {
		t.Fatalf("the table still counts %d senders after everything was swept", st.Senders)
	}
}

// TestWriteFailureLeavesNothingInMemory: if the durable write fails, nothing was
// acknowledged and nothing may be visible.
func TestWriteFailureLeavesNothingInMemory(t *testing.T) {
	lg := &fakeLog{err: errors.New("disk is on fire")}
	s := NewStore(Options{})
	if err := s.Attach(lg); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := s.Accept(testKey, testSender, testRecipient); err == nil {
		t.Fatal("Accept returned nil over a failing log")
	}
	if _, ok := s.Lookup(testKey, testRecipient); ok {
		t.Fatal("a row is visible after its durable write failed; nothing unacknowledged may be visible (invariant 5)")
	}
	// The reservation must have been released, or a failed write would leak a
	// slot for the life of the process.
	if st := s.Stats(); st.Entries != 0 {
		t.Fatalf("the table reports %d entries after a failed write", st.Entries)
	}
	if err := s.Accept(testKey, testSender, testRecipient); err == nil {
		t.Fatal("a second attempt succeeded over a failing log")
	}
}

// TestConcurrentAcceptIsRaceFree exercises the lock discipline the whole design
// rests on: the lock is never held across a durable write, and the reservation
// is what keeps the bound honest across that window. Run with -race.
func TestConcurrentAcceptIsRaceFree(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// +1 because a message sequence starts at 1: "testbus-0" is not a
			// well-formed message id and every Accept for it would be refused,
			// which would make this test quietly exercise three keys.
			key := fmt.Sprintf("testbus-%d", i%4+1)
			_ = s.Accept(key, testSender, testRecipient)
			_ = s.MarkInFlight(key, testRecipient)
			_, _ = s.Lookup(key, testRecipient)
			_ = s.Stats()
		}(i)
	}
	wg.Wait()
	if n := s.Len(); n != 4 {
		t.Fatalf("the table holds %d rows for 4 distinct keys, want 4", n)
	}
}

// TestValidateCorrelationKey pins the rule a future status API depends on: a
// client-supplied key is INPUT TO BE VALIDATED, never an identity to be trusted
// (invariant 1).
func TestValidateCorrelationKey(t *testing.T) {
	if err := ValidateCorrelationKey(testKey); err != nil {
		t.Fatalf("ValidateCorrelationKey(%q) = %v, want nil", testKey, err)
	}
	for _, bad := range []string{"", "nope", strings.Repeat("a", 500), "testbus.agent-1"} {
		if err := ValidateCorrelationKey(bad); err == nil {
			t.Errorf("ValidateCorrelationKey(%q) = nil, want a refusal", bad)
		} else if len(err.Error()) > 400 {
			t.Errorf("the refusal for a %d-byte input is %d bytes; untrusted text must be elided before it is quoted", len(bad), len(err.Error()))
		}
	}
}

// gateLog blocks inside Write, once, so a test can hold one caller between its
// decision and its fsync and drive a second caller into that exact window.
//
// It is what makes the concurrency test DETERMINISTIC rather than a race the
// suite might or might not lose. A `go func` plus a sleep would pass on a build
// where the reservation was deleted, roughly half the time.
type gateLog struct {
	inner   *fakeLog
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func newGateLog() *gateLog {
	return &gateLog{inner: &fakeLog{}, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *gateLog) Write(e wal.Entry) (wal.Committed, error) {
	if g.armed {
		g.armed = false
		g.entered <- struct{}{}
		<-g.release
	}
	return g.inner.Write(e)
}

// TestConcurrentConflictingSettleIsRejected is ACK-CONTRACT.md §8.2 note 4 under
// concurrency, and it is the case the "is it already terminal?" check ALONE
// cannot cover.
//
// The lock is released before the durable write — it must be, since wal.Log.Write
// calls Applier.Apply synchronously and this Store IS that applier. So without a
// per-pair reservation two callers offering DIFFERENT terminal outcomes both read
// the same non-terminal row, both pass the check, and both are told success. The
// first terminal still wins in memory and on replay, so nothing is corrupted —
// which is exactly why this needs a test rather than being caught by one: the
// visible symptoms are a caller told its contradicting outcome stood, and an
// ERROR discard logged by every future replay for a record that was never wrong.
func TestConcurrentConflictingSettleIsRejected(t *testing.T) {
	g := newGateLog()
	s := NewStore(Options{})
	if err := s.Attach(g); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	mustAccept(t, s, testRecipient)

	// The next write — the first caller's `delivered` — parks inside Write, with
	// the pair reserved and mu released.
	g.armed = true
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- s.Settle(testKey, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified)
	}()
	<-g.entered

	// The second caller arrives in the window, offering a CONTRADICTING terminal.
	second := s.Settle(testKey, testRecipient, StateRefused, ClassRecipientRefusedPolicy, AttestedByRecipientSignatureUnverified)
	if second == nil {
		t.Fatal("a contradicting terminal offered while another is being made durable returned nil; both callers were told their outcome stood, and only one of them can be true (ACK-CONTRACT.md §8.2 note 4)")
	}
	if !errors.Is(second, ErrConcurrentTransition) && !errors.Is(second, ErrTerminal) {
		t.Fatalf("the second terminal was refused with %v, want ErrConcurrentTransition or ErrTerminal", second)
	}

	close(g.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("the FIRST terminal returned %v, want nil: it won the reservation and its record is durable", err)
	}

	// One accept plus ONE settle. A second, contradicting settle record in an
	// append-only log is a record every future replay must discard at ERROR.
	written := g.inner.written()
	if len(written) != 2 {
		t.Fatalf("%d records were written, want 2 (one accept, one settle): a rejected transition must write NOTHING", len(written))
	}
	r, _ := s.Lookup(testKey, testRecipient)
	if r.State != StateDelivered {
		t.Fatalf("the row is %s, want delivered: the FIRST terminal stands", r.State)
	}
	// And a replay of exactly those bytes reaches the same state with no discard.
	rebuilt := g.inner.replayFrom(t, Options{})
	if got, _ := rebuilt.Lookup(testKey, testRecipient); got.State != StateDelivered {
		t.Fatalf("a replay of the written bytes yields %s, want delivered", got.State)
	}
}

// TestConcurrentInFlightAndSettleCannotWriteAStaleRecord is the same reservation
// seen from the other side: a MarkInFlight parked between its decision and its
// fsync must not be able to append a stale `in_flight` record after a concurrent
// terminal one.
//
// Under commit-order replay that record would arrive AFTER the terminal, be
// refused as a rank regression, and be logged as an ERROR discard on every start
// for the life of the row.
func TestConcurrentInFlightAndSettleCannotWriteAStaleRecord(t *testing.T) {
	g := newGateLog()
	s := NewStore(Options{})
	if err := s.Attach(g); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	mustAccept(t, s, testRecipient)

	g.armed = true
	firstDone := make(chan error, 1)
	go func() { firstDone <- s.MarkInFlight(testKey, testRecipient) }()
	<-g.entered

	if err := s.Settle(testKey, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); !errors.Is(err, ErrConcurrentTransition) {
		t.Fatalf("a settle offered while an in-flight transition is being made durable = %v, want ErrConcurrentTransition", err)
	}
	close(g.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("MarkInFlight returned %v, want nil", err)
	}
	if len(g.inner.written()) != 2 {
		t.Fatalf("%d records were written, want 2", len(g.inner.written()))
	}
}

// TestMutatingMethodsValidateBeforeTheyLookUpOrLog: Settle and MarkInFlight are
// called by ROUTES (ACK-4, ACK-6), so both strings are remote input. Invariant 1
// makes a client-supplied correlation key input to be VALIDATED rather than an
// identity to be trusted, and an unbounded one must never be echoed verbatim
// into an error string or an operator log line.
func TestMutatingMethodsValidateBeforeTheyLookUpOrLog(t *testing.T) {
	s, lg := newTestStore(t, Options{})
	mustAccept(t, s, testRecipient)

	hostile := strings.Repeat("A", 1<<16)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Settle/key", func() error {
			return s.Settle(hostile, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified)
		}},
		{"Settle/recipient", func() error {
			return s.Settle(testKey, hostile, StateDelivered, "", AttestedByRecipientSignatureUnverified)
		}},
		{"MarkInFlight/key", func() error { return s.MarkInFlight(hostile, testRecipient) }},
		{"MarkInFlight/recipient", func() error { return s.MarkInFlight(testKey, hostile) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("= %v, want ErrInvalidRecord — validation must happen BEFORE the lookup", err)
			}
			if len(err.Error()) > 400 {
				t.Fatalf("the refusal is %d bytes for a %d-byte input; untrusted text must be elided before it is quoted", len(err.Error()), len(hostile))
			}
		})
	}
	if n := len(lg.written()); n != 1 {
		t.Fatalf("%d records were written, want 1 (the accept): a refused call must write nothing", n)
	}
}

// TestSweepIsNotOccupancyLinear is a DENIAL-OF-SERVICE regression guard, not a
// performance test.
//
// Every exported entry point sweeps, and one production Accept sweeps three
// times (its own, Apply's during the live wal write, and foldIn's) — all inside
// Hub.publish with the GLOBAL WRITE LOCK held. An earlier revision ranged the
// whole map, which made every send on the bus pay for the table's occupancy:
// 13.9 ms per Accept at the 65536-row cap against 107 µs at 1024, with no fsync
// in the measurement at all. One authenticated agent reaching its own fair share
// taxed every OTHER agent's sends for the full 24h window at zero marginal cost
// to itself.
//
// The assertion is on WORK, not on wall-clock time: a timing assertion would be
// flaky on a loaded box and would not say what it means. sweptEntries counts
// expiry-queue entries popped, so "the sweep did no work at 4000 rows either" is
// exactly the property, stated exactly.
func TestSweepIsNotOccupancyLinear(t *testing.T) {
	now := testAccepted
	s, _ := newTestStore(t, Options{MaxEntries: 20000, Now: func() time.Time { return now }})

	fill := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			if err := s.Accept(fmt.Sprintf("testbus-%d", i), fmt.Sprintf("testbus.s%d-1", i%64), testRecipient); err != nil {
				t.Fatalf("Accept %d: %v", i, err)
			}
		}
	}

	fill(1, 1001)
	s.mu.Lock()
	s.sweptEntries = 0
	s.mu.Unlock()
	if err := s.Accept("testbus-90001", testSender, testRecipient); err != nil {
		t.Fatalf("Accept at 1000 rows: %v", err)
	}
	s.mu.Lock()
	atThousand := s.sweptEntries
	s.mu.Unlock()

	fill(1001, 5001)
	s.mu.Lock()
	s.sweptEntries = 0
	s.mu.Unlock()
	if err := s.Accept("testbus-90002", testSender, testRecipient); err != nil {
		t.Fatalf("Accept at 5000 rows: %v", err)
	}
	s.mu.Lock()
	// queued counts LIVE entries: expiry carries a dead prefix of popped,
	// zeroed slots (Store.expiryHead), and the "at most 2 per row" bound below
	// is a statement about entries the sweep can still reach, not about the
	// allocation holding them. Nothing has been popped at this point in the
	// test, so head is 0 and the two agree — subtracting it keeps the assertion
	// true of what it claims if that ever stops being the case.
	atFiveThousand, retained, queued := s.sweptEntries, len(s.records), len(s.expiry)-s.expiryHead
	s.mu.Unlock()

	if atThousand != 0 || atFiveThousand != 0 {
		t.Fatalf("one Accept popped %d expiry entries at 1000 rows and %d at 5000, want 0 and 0: with nothing expired the sweep must stop at the first live entry, not walk the table",
			atThousand, atFiveThousand)
	}
	if retained < 5000 {
		t.Fatalf("the table holds %d rows, want >= 5000: this guard proved nothing if the rows were not admitted", retained)
	}
	// The queue must stay bounded by the table, or "O(expired)" would just move
	// the unbounded growth somewhere else. A row is queued once on insert and
	// once more only when it settles.
	if queued > 2*retained {
		t.Fatalf("the expiry queue holds %d entries for %d rows, want at most 2 per row", queued, retained)
	}

	// And the sweep must still actually sweep: when the window passes, one call
	// retires everything, and the queue is compacted with it.
	now = testAccepted.Add(Retention)
	if n := s.Len(); n != 0 {
		t.Fatalf("the table holds %d rows past the retention window, want 0", n)
	}
	s.mu.Lock()
	// A fully-drained queue releases its backing array outright, so this checks
	// the whole slice rather than the live region: len 0 and head 0 both.
	leftover := len(s.expiry)
	if s.expiryHead != 0 {
		t.Errorf("the expiry queue head is %d after everything was swept, want 0: a drained queue must reset its front, not keep an offset into a released array", s.expiryHead)
	}
	s.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("the expiry queue holds %d entries after everything was swept, want 0: a queue that never shrinks is the leak the sweep exists to prevent", leftover)
	}
}

// TestSettleRequeuesTheTerminalAnchor pins the one case where a row is queued
// twice. §11 measures a non-terminal row from accepted_at and a terminal one
// from settled_at, so settling moves the deadline LATER — and a row whose new
// anchor was never queued would be swept on its OLD deadline, retiring a
// terminal outcome early and reporting `unknown` about something the bus knows.
func TestSettleRequeuesTheTerminalAnchor(t *testing.T) {
	now := testAccepted
	s, _ := newTestStore(t, Options{Now: func() time.Time { return now }})
	mustAccept(t, s, testRecipient)

	now = testAccepted.Add(12 * time.Hour)
	if err := s.Settle(testKey, testRecipient, StateDelivered, "", AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	// Past the ACCEPTED deadline but not the SETTLED one: the row must survive.
	now = testAccepted.Add(Retention + time.Minute)
	r, ok := s.Lookup(testKey, testRecipient)
	if !ok {
		t.Fatal("a terminal row was swept on its accepted_at deadline; a terminal row ages from settled_at (§11)")
	}
	if r.State != StateDelivered {
		t.Fatalf("the row is %s, want delivered", r.State)
	}
	// Past the SETTLED deadline: gone.
	now = testAccepted.Add(12*time.Hour + Retention)
	if _, ok := s.Lookup(testKey, testRecipient); ok {
		t.Fatal("a terminal row outlived its settled_at deadline")
	}
	// A row that transitioned but did NOT settle must NOT be re-queued, or the
	// queue would grow per transition rather than per row.
	s2, _ := newTestStore(t, Options{Now: func() time.Time { return now }})
	if err := s2.Accept(testKey, testSender, testRecipient); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := s2.MarkInFlight(testKey, testRecipient); err != nil {
			t.Fatalf("MarkInFlight: %v", err)
		}
	}
	s2.mu.Lock()
	// LIVE entries, for the same reason as in TestSweepIsNotOccupancyLinear:
	// the assertion below is about entries the sweep can still reach, not about
	// the allocation holding them. Nothing has expired on s2, so head is 0 and
	// the two agree today.
	queued := len(s2.expiry) - s2.expiryHead
	s2.mu.Unlock()
	if queued != 1 {
		t.Fatalf("the expiry queue holds %d entries after one accept and five in-flight transitions, want 1: only a state change that MOVES THE ANCHOR may re-queue", queued)
	}
}
