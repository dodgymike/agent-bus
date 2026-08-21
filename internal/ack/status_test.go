package ack

import (
	"fmt"
	"testing"
	"time"
)

// otherSender is a DIFFERENT principal on the same bus. Same bus half on
// purpose: a filter that compared only the bus prefix, or only the agent half,
// would still pass a test that used two agents on two different buses.
const otherSender = "testbus.gamma-1"

// TestStatusRowsFiltersBySender is the §13.3 guard: only the ORIGINAL SENDER
// sees a row, and every other case is the SAME empty answer.
//
// # MUTATION PROOF
//
// Deleting the `if r.Sender != sender { continue }` line in StatusRows makes the
// "another sender" subtest FAIL with 1 row where 0 were wanted. Weakening the
// comparison to a bus-prefix match (strings.HasPrefix on "testbus.") fails it
// the same way, because both principals are on testbus. Verified by running
// both mutations, not by reading the code.
func TestStatusRowsFiltersBySender(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	mustAccept(t, s, testRecipient)

	if got := s.StatusRows(testKey, testSender); len(got) != 1 {
		t.Fatalf("the sender's own key returned %d rows, want 1 — the accessor is not returning what it must", len(got))
	} else if got[0].Recipient != testRecipient || got[0].State != StateAccepted {
		t.Fatalf("row = %+v, want recipient %q in state accepted", got[0], testRecipient)
	}

	for _, tc := range []struct {
		name, key, sender string
	}{
		// The four §13.3 cases, which must be indistinguishable to a caller.
		{"another sender's key", testKey, otherSender},
		{"a key that never existed", "testbus-999", testSender},
		{"a malformed key", "not a message id", testSender},
		{"a malformed sender", testKey, "unqualified"},
		{"an empty key", "", testSender},
		{"an empty sender", testKey, ""},
		{"the recipient asking about a message sent TO it", testKey, testRecipient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s.StatusRows(tc.key, tc.sender)
			if got != nil {
				t.Fatalf("StatusRows(%q, %q) returned %d rows (%+v); §13.3 requires the SAME empty answer as a key that never existed — anything else is an existence oracle",
					tc.key, tc.sender, len(got), got)
			}
		})
	}
}

// TestStatusRowsGoesUnknownAfterSweep: retention is a promise about how long an
// answer is AVAILABLE, and serving a row past its window would quietly extend
// it. A swept row must read exactly like one that never existed.
//
// MUTATION: removing the s.sweepLocked(s.now()) call from StatusRows makes this
// FAIL — the expired row is still returned.
func TestStatusRowsGoesUnknownAfterSweep(t *testing.T) {
	now := time.Now()
	s, _ := newTestStore(t, Options{
		Retention: time.Hour,
		Now:       func() time.Time { return now },
	})
	mustAccept(t, s, testRecipient)
	if got := s.StatusRows(testKey, testSender); len(got) != 1 {
		t.Fatalf("before the window elapsed: %d rows, want 1", len(got))
	}

	now = now.Add(2 * time.Hour)
	if got := s.StatusRows(testKey, testSender); got != nil {
		t.Fatalf("after the retention window: %d rows, want none — a swept row must be indistinguishable from one that never existed", len(got))
	}
}

// TestStatusRowsIndexIsBoundedByTheTable: the correlation index must hold
// exactly the keys of the table and nothing more. An index that only ever grew
// would be a leak whose entire purpose was to be bounded.
//
// MUTATION: removing the s.indexRemoveLocked(k) call from delLocked makes this
// FAIL at the final length check with 3 keys retained for 0 rows.
func TestStatusRowsIndexIsBoundedByTheTable(t *testing.T) {
	now := time.Now()
	s, _ := newTestStore(t, Options{
		Retention: time.Hour,
		Now:       func() time.Time { return now },
	})
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("testbus-%d", 100+i)
		if err := s.Accept(key, testSender, testRecipient); err != nil {
			t.Fatalf("Accept(%s): %v", key, err)
		}
	}
	s.mu.Lock()
	held := len(s.byCorrelation)
	s.mu.Unlock()
	if held != 3 {
		t.Fatalf("the correlation index holds %d keys for 3 rows, want 3", held)
	}

	now = now.Add(2 * time.Hour)
	if n := s.Len(); n != 0 {
		t.Fatalf("after the retention window the table holds %d rows, want 0", n)
	}
	s.mu.Lock()
	held = len(s.byCorrelation)
	s.mu.Unlock()
	if held != 0 {
		t.Fatalf("the correlation index holds %d keys for 0 retained rows; it is not bounded by the table it indexes", held)
	}
}

// TestStatusRowsSurvivesTransitions: a transition REPLACES a row rather than
// inserting one, and the index must not double-count or lose it. It also pins
// the §13.2 fields a terminal row must carry.
//
// MUTATION: moving s.indexAddLocked(k) OUT of putLocked's !existed branch does
// not break this (a set add is idempotent), which is why the bound above is
// asserted separately — that pair is what actually holds the index correct.
func TestStatusRowsSurvivesTransitions(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	mustAccept(t, s, testRecipient)

	if err := s.MarkInFlight(testKey, testRecipient); err != nil {
		t.Fatalf("MarkInFlight: %v", err)
	}
	rows := s.StatusRows(testKey, testSender)
	if len(rows) != 1 || rows[0].State != StateInFlight {
		t.Fatalf("after MarkInFlight: %+v, want exactly one in_flight row", rows)
	}

	if err := s.Settle(testKey, testRecipient, StateRefused, ClassRecipientRefusedPolicy, AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	rows = s.StatusRows(testKey, testSender)
	if len(rows) != 1 {
		t.Fatalf("after Settle: %d rows, want 1", len(rows))
	}
	r := rows[0]
	switch {
	case r.State != StateRefused:
		t.Errorf("state = %s, want refused", r.State)
	case r.Class != ClassRecipientRefusedPolicy:
		t.Errorf("class = %q, want %q", r.Class, ClassRecipientRefusedPolicy)
	case r.AttestedBy != AttestedByRecipientSignatureUnverified:
		t.Errorf("attested_by = %q, want %q", r.AttestedBy, AttestedByRecipientSignatureUnverified)
	case r.SettledAt.IsZero():
		t.Error("a terminal row carries no settled_at; it could then never be swept")
	}
}

// TestStatusRowsAreOrderedByRecipient: map iteration order is randomised in Go,
// and an ordering that changed between two identical requests would look like
// the state had changed when it had not.
func TestStatusRowsAreOrderedByRecipient(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	for _, r := range []string{"testbus.zeta-1", "testbus.beta-1", "testbus.mu-1"} {
		if err := s.Accept(testKey, testSender, r); err != nil {
			t.Fatalf("Accept(%s): %v", r, err)
		}
	}
	want := []string{"testbus.beta-1", "testbus.mu-1", "testbus.zeta-1"}
	for i := 0; i < 8; i++ {
		rows := s.StatusRows(testKey, testSender)
		if len(rows) != len(want) {
			t.Fatalf("got %d rows, want %d", len(rows), len(want))
		}
		for j, w := range want {
			if rows[j].Recipient != w {
				t.Fatalf("row %d = %q, want %q (iteration %d) — the order is not stable", j, rows[j].Recipient, w, i)
			}
		}
	}
}
