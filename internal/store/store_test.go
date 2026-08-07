// Package store_test exercises the serving copy through its exported surface.
package store_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/store"
)

const testBusID = "testbus"

// testClock is an injected clock. Retention is a function of TIME, and a test
// that slept for it would be both slow and flaky; this makes the passage of
// time an explicit, deterministic input.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *testClock {
	return &testClock{t: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)}
}

// noEpoch is the ZERO enrolment epoch, which disables the enrolment-epoch
// filter in store.Message.VisibleTo.
//
// It is what a ROSTER-LESS caller passes — an operator dump, an audit tool —
// and it must never stand in for a request path, where the epoch comes from the
// authenticated principal's roster entry. It is used below only where the
// property under test is retention or addressing rather than the epoch itself;
// the epoch has its own coverage in TestStoreEnrolmentEpoch, and the visibility
// table in TestStoreSince exercises a real, non-zero epoch on both sides of the
// boundary.
var noEpoch time.Time

func agentIDFor(t *testing.T, name string) string {
	t.Helper()
	id, err := ids.AgentID(testBusID, name, 1)
	if err != nil {
		t.Fatalf("ids.AgentID(%q, %q, 1): %v", testBusID, name, err)
	}
	return id
}

// mkMessage builds a valid Message through the only constructor, so the
// interdependent fields (id from seq, hash from body) cannot drift.
func mkMessage(t *testing.T, sender string, broadcast bool, recipients []string, seq uint64, sentAt time.Time, body string) store.Message {
	t.Helper()
	m, err := store.NewMessage(testBusID, sender, broadcast, recipients, seq, sentAt, []byte(body), fmt.Sprintf("key-%d", seq), testTimestampMs, testSignature(t))
	if err != nil {
		t.Fatalf("store.NewMessage(seq=%d): %v", seq, err)
	}
	return m
}

func mustAppend(t *testing.T, s *store.Store, m store.Message) {
	t.Helper()
	if err := s.Append(m); err != nil {
		t.Fatalf("Append(seq=%d): %v", m.Seq, err)
	}
}

// ---------------------------------------------------------------------------

func TestStoreAppendOrdering(t *testing.T) {
	clock := newClock()
	s := store.New(store.Options{Now: clock.now})
	a := agentIDFor(t, "alpha")

	mustAppend(t, s, mkMessage(t, a, true, nil, 1, clock.now(), "one"))
	mustAppend(t, s, mkMessage(t, a, true, nil, 7, clock.now(), "seven"))

	if got := s.Head(); got != 7 {
		t.Fatalf("Head() = %d after appending 1 then 7, want 7", got)
	}

	cases := []struct {
		name string
		seq  uint64
		want error
	}{
		{"Zero", 0, store.ErrInvalidMessage},
		{"BehindHead", 3, store.ErrOutOfOrder},
		{"AtHead", 7, store.ErrOutOfOrder},
	}
	if len(cases) == 0 {
		t.Fatal("the out-of-order table is empty")
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			m := mkMessage(t, a, true, nil, 1, clock.now(), "x")
			m.Seq = c.seq // deliberately desynchronised from the id: Append is the check
			err := s.Append(m)
			if !errors.Is(err, c.want) {
				t.Fatalf("Append(seq=%d) = %v, want %v", c.seq, err, c.want)
			}
			if got := s.Head(); got != 7 {
				t.Fatalf("a rejected Append moved the head to %d, want 7", got)
			}
			if count, _, _, _, _ := s.Stats(); count != 2 {
				t.Fatalf("a rejected Append changed the retained count to %d, want 2", count)
			}
		})
	}

	// A GAP is legitimate: the bus burns sequence numbers on a discarded prepare
	// and must never compact them away.
	mustAppend(t, s, mkMessage(t, a, true, nil, 100, clock.now(), "hundred"))
	if got := s.Head(); got != 100 {
		t.Fatalf("Head() = %d, want 100", got)
	}
}

func TestStoreSince(t *testing.T) {
	clock := newClock()
	s := store.New(store.Options{Now: clock.now})
	a := agentIDFor(t, "alpha")
	b := agentIDFor(t, "beta")
	g := agentIDFor(t, "gamma")

	// 1 broadcast(a), 2 dm a->b, 3 broadcast(b), 4 dm a->g, 5 broadcast(a)
	mustAppend(t, s, mkMessage(t, a, true, nil, 1, clock.now(), "m1"))
	mustAppend(t, s, mkMessage(t, a, false, []string{b}, 2, clock.now(), "m2"))
	mustAppend(t, s, mkMessage(t, b, true, nil, 3, clock.now(), "m3"))
	mustAppend(t, s, mkMessage(t, a, false, []string{g}, 4, clock.now(), "m4"))
	mustAppend(t, s, mkMessage(t, a, true, nil, 5, clock.now(), "m5"))

	t.Run("FiltersByPrincipal", func(t *testing.T) {
		// Every fixture message above was sent at clock.now(). enrolledEarly
		// precedes all of them, enrolledLate follows all of them.
		enrolledEarly := clock.now().Add(-time.Minute)
		enrolledLate := clock.now().Add(time.Minute)

		cases := []struct {
			name       string
			agent      string
			enrolledAt time.Time
			want       []uint64
		}{
			{"SenderExcludedFromItsOwnSends", a, enrolledEarly, []uint64{3}},
			{"RecipientSeesBroadcastsAndItsDM", b, enrolledEarly, []uint64{1, 2, 5}},
			{"ThirdPartySeesBothBroadcastsPlusItsDM", g, enrolledEarly, []uint64{1, 3, 4, 5}},
			{"EmptyPrincipalSeesNothing", "", enrolledEarly, nil},
			// A later arrival reads back through the retention window — but only
			// as far as its own enrolment.
			{"LaterArrivalReadsBackThroughTheWindow", agentIDFor(t, "delta"), enrolledEarly, []uint64{1, 3, 5}},
			// THE ENROLMENT EPOCH. Enrolled after every message on the bus, so
			// none of it is deliverable, not even the DM addressed to this id.
			{"EnrolledAfterEverythingSeesNothing", b, enrolledLate, nil},
			{"EnrolledAfterEverythingSeesNoBroadcastEither", g, enrolledLate, nil},
			// A roster-less audit read passes the zero epoch, which disables the
			// check; addressing still applies.
			{"ZeroEpochDisablesTheCheckButNotAddressing", b, noEpoch, []uint64{1, 2, 5}},
		}
		if len(cases) == 0 {
			t.Fatal("the visibility table is empty")
		}
		checked := 0
		for _, c := range cases {
			batch, next, more := s.Since(c.agent, c.enrolledAt, 0, 100)
			if len(batch) != len(c.want) {
				t.Fatalf("%s: Since(%q, %v, 0, 100) returned %v, want %v", c.name, c.agent, c.enrolledAt, seqsOf(batch), c.want)
			}
			for i, m := range batch {
				if m.Seq != c.want[i] {
					t.Fatalf("%s: Since(%q)[%d] has sequence %d, want %d", c.name, c.agent, i, m.Seq, c.want[i])
				}
			}
			if more {
				t.Fatalf("%s: Since(%q, 0, 100) reported more with a limit of 100", c.name, c.agent)
			}
			if len(c.want) == 0 {
				if next != 0 {
					t.Fatalf("%s: Since(%q) with an empty batch moved the cursor to %d", c.name, c.agent, next)
				}
			} else if next != c.want[len(c.want)-1] {
				t.Fatalf("%s: Since(%q) returned cursor %d, want %d", c.name, c.agent, next, c.want[len(c.want)-1])
			}
			checked++
		}
		if checked != len(cases) {
			t.Fatalf("checked %d principals, want %d", checked, len(cases))
		}
	})

	t.Run("Pagination", func(t *testing.T) {
		// gamma sees 1, 3, 4, 5. more reports "the batch was CUT SHORT and another
		// call returns immediately", so it is false on the second round: that
		// batch filled the limit exactly and nothing visible follows it.
		want := [][]uint64{{1, 3}, {4, 5}, {}}
		wantMore := []bool{true, false, false}
		after := uint64(0)
		delivered := 0
		for round := range want {
			batch, next, more := s.Since(g, noEpoch, after, 2)
			if len(batch) != len(want[round]) {
				t.Fatalf("round %d returned %d messages, want %d", round, len(batch), len(want[round]))
			}
			for i, m := range batch {
				if m.Seq != want[round][i] {
					t.Fatalf("round %d[%d] has sequence %d, want %d", round, i, m.Seq, want[round][i])
				}
				delivered++
			}
			if more != wantMore[round] {
				t.Fatalf("round %d: more = %v, want %v", round, more, wantMore[round])
			}
			if len(batch) == 0 && next != after {
				t.Fatalf("round %d: an EMPTY batch moved the cursor from %d to %d", round, after, next)
			}
			after = next
		}
		if delivered != 4 {
			t.Fatalf("pagination delivered %d messages, want 4", delivered)
		}
	})

	t.Run("NonPositiveLimitReturnsNothingAndKeepsTheCursor", func(t *testing.T) {
		// store.Since does no clamping of its own — that is the hub's job. What
		// it must NOT do is advance a cursor past messages it never handed over.
		for _, limit := range []int{0, -1} {
			batch, next, more := s.Since(g, noEpoch, 2, limit)
			if len(batch) != 0 || more {
				t.Fatalf("Since(limit=%d) returned %d messages (more=%v), want none", limit, len(batch), more)
			}
			if next != 2 {
				t.Fatalf("Since(limit=%d) moved the cursor from 2 to %d", limit, next)
			}
		}
	})

	t.Run("HasVisibleAfter", func(t *testing.T) {
		enrolledEarly := clock.now().Add(-time.Minute)
		enrolledLate := clock.now().Add(time.Minute)

		cases := []struct {
			agent      string
			enrolledAt time.Time
			after      uint64
			want       bool
		}{
			{g, enrolledEarly, 0, true},
			{g, enrolledEarly, 4, true},  // 5 is still ahead
			{g, enrolledEarly, 5, false}, // caught up
			{g, enrolledEarly, 99, false},
			{a, enrolledEarly, 3, false}, // everything after 3 is a's own
			{a, enrolledEarly, 0, true},
			{"", enrolledEarly, 0, false},
			{agentIDFor(t, "nobody"), enrolledEarly, 0, true}, // broadcasts are visible to anyone
			// The epoch is applied by the PARK predicate too, not just by the
			// batch read — otherwise a long poll would park, be woken, and then
			// be handed nothing.
			{g, enrolledLate, 0, false},
			{b, enrolledLate, 0, false},
			{agentIDFor(t, "nobody"), enrolledLate, 0, false},
			{g, noEpoch, 0, true},
		}
		if len(cases) == 0 {
			t.Fatal("the HasVisibleAfter table is empty")
		}
		checked := 0
		for _, c := range cases {
			if got := s.HasVisibleAfter(c.agent, c.enrolledAt, c.after); got != c.want {
				t.Fatalf("HasVisibleAfter(%q, %v, %d) = %v, want %v", c.agent, c.enrolledAt, c.after, got, c.want)
			}
			checked++
		}
		if checked != len(cases) {
			t.Fatalf("checked %d HasVisibleAfter cases, want %d", checked, len(cases))
		}
	})

	t.Run("Stats", func(t *testing.T) {
		count, bytes, oldest, head, dropped := s.Stats()
		if count != 5 {
			t.Fatalf("Stats count = %d, want 5", count)
		}
		if want := int64(5 * len("m1")); bytes != want {
			t.Fatalf("Stats bytes = %d, want %d", bytes, want)
		}
		if oldest != 1 {
			t.Fatalf("Stats oldest = %d, want 1", oldest)
		}
		if head != 5 {
			t.Fatalf("Stats head = %d, want 5", head)
		}
		if dropped != 0 {
			t.Fatalf("Stats dropped = %d, want 0", dropped)
		}
	})
}

func TestStoreRetention(t *testing.T) {
	t.Run("ByAge", func(t *testing.T) {
		clock := newClock()
		s := store.New(store.Options{MaxAge: time.Hour, Now: clock.now})
		a := agentIDFor(t, "alpha")
		b := agentIDFor(t, "beta")

		// Three messages, ten minutes apart. SentAt is what retention measures
		// against, not an insertion timestamp.
		base := clock.now()
		for i := uint64(1); i <= 3; i++ {
			mustAppend(t, s, mkMessage(t, a, true, nil, i, base.Add(time.Duration(i)*10*time.Minute), fmt.Sprintf("m%d", i)))
		}
		if count, _, _, _, dropped := s.Stats(); count != 3 || dropped != 0 {
			t.Fatalf("before any time passes: count=%d dropped=%d, want 3 and 0", count, dropped)
		}

		// Move the clock so the first message is exactly at the cutoff. Pruning
		// uses SentAt.Before(cutoff), so "exactly at" is RETAINED.
		clock.t = base.Add(10*time.Minute + time.Hour)
		if got := len(mustSince(t, s, b, 0, 100)); got != 3 {
			t.Fatalf("a message exactly at the age cutoff was dropped: %d retained, want 3", got)
		}

		// One nanosecond further and it goes.
		clock.advance(time.Nanosecond)
		batch := mustSince(t, s, b, 0, 100)
		if len(batch) != 2 || batch[0].Seq != 2 {
			t.Fatalf("after the cutoff passed, retained = %v, want sequences 2 and 3", seqsOf(batch))
		}
		count, _, oldest, head, dropped := s.Stats()
		if count != 2 || oldest != 2 || head != 3 || dropped != 1 {
			t.Fatalf("Stats after one age-drop = (count %d, oldest %d, head %d, dropped %d), want (2, 2, 3, 1)", count, oldest, head, dropped)
		}

		// Head SURVIVES pruning: a caught-up cursor stays caught up even when
		// everything behind it has aged out.
		clock.advance(24 * time.Hour)
		if got := len(mustSince(t, s, b, 0, 100)); got != 0 {
			t.Fatalf("everything should have aged out, %d retained", got)
		}
		if got := s.Head(); got != 3 {
			t.Fatalf("Head() = %d after everything aged out, want 3 — the head must survive pruning", got)
		}
		if count, bytes, oldest, _, dropped := s.Stats(); count != 0 || bytes != 0 || oldest != 0 || dropped != 3 {
			t.Fatalf("Stats on an emptied store = (count %d, bytes %d, oldest %d, dropped %d), want (0, 0, 0, 3)", count, bytes, oldest, dropped)
		}
	})

	t.Run("ByBytes", func(t *testing.T) {
		clock := newClock()
		// Room for exactly two 10-byte bodies.
		s := store.New(store.Options{MaxAge: 24 * time.Hour, MaxBytes: 20, Now: clock.now})
		a := agentIDFor(t, "alpha")
		b := agentIDFor(t, "beta")

		for i := uint64(1); i <= 4; i++ {
			mustAppend(t, s, mkMessage(t, a, true, nil, i, clock.now(), "0123456789"))
		}
		batch := mustSince(t, s, b, 0, 100)
		if len(batch) != 2 || batch[0].Seq != 3 || batch[1].Seq != 4 {
			t.Fatalf("byte retention kept %v, want the newest two (3, 4)", seqsOf(batch))
		}
		count, bytes, oldest, head, dropped := s.Stats()
		if count != 2 || bytes != 20 || oldest != 3 || head != 4 || dropped != 2 {
			t.Fatalf("Stats = (count %d, bytes %d, oldest %d, head %d, dropped %d), want (2, 20, 3, 4, 2)", count, bytes, oldest, head, dropped)
		}
	})

	t.Run("WhicheverBoundComesFirst", func(t *testing.T) {
		// Both bounds are enforced together and the TIGHTER one wins. Age is
		// tight here and bytes are not; the byte ceiling must not resurrect what
		// age dropped, and the count must reflect age alone.
		clock := newClock()
		s := store.New(store.Options{MaxAge: time.Hour, MaxBytes: 1 << 20, Now: clock.now})
		a := agentIDFor(t, "alpha")
		b := agentIDFor(t, "beta")

		base := clock.now()
		for i := uint64(1); i <= 4; i++ {
			mustAppend(t, s, mkMessage(t, a, true, nil, i, base.Add(time.Duration(i)*30*time.Minute), "body"))
		}
		clock.t = base.Add(2*time.Hour + time.Minute) // cutoff = base + 1h1m
		batch := mustSince(t, s, b, 0, 100)
		if len(batch) != 2 || batch[0].Seq != 3 {
			t.Fatalf("age won over a slack byte bound: retained %v, want (3, 4)", seqsOf(batch))
		}

		// And the other way round: bytes tight, age slack.
		clock2 := newClock()
		s2 := store.New(store.Options{MaxAge: 100 * time.Hour, MaxBytes: 4, Now: clock2.now})
		for i := uint64(1); i <= 3; i++ {
			mustAppend(t, s2, mkMessage(t, a, true, nil, i, clock2.now(), "body"))
		}
		batch2 := mustSince(t, s2, b, 0, 100)
		if len(batch2) != 1 || batch2[0].Seq != 3 {
			t.Fatalf("bytes won over a slack age bound: retained %v, want just (3)", seqsOf(batch2))
		}
	})

	t.Run("PruningKeepsTheBinarySearchHonest", func(t *testing.T) {
		// A cursor that has fallen off the back of the retention window must
		// resume at the OLDEST RETAINED message — not at the end, and not by
		// re-delivering things it already had. sort.Search over a pruned slice is
		// where an off-by-one here would show up.
		clock := newClock()
		s := store.New(store.Options{MaxAge: 24 * time.Hour, MaxBytes: 30, Now: clock.now})
		a := agentIDFor(t, "alpha")
		b := agentIDFor(t, "beta")

		for i := uint64(1); i <= 10; i++ {
			mustAppend(t, s, mkMessage(t, a, true, nil, i, clock.now(), "0123456789"))
		}
		// Room for three; sequences 8, 9, 10 remain.
		retained := mustSince(t, s, b, 0, 100)
		if len(retained) != 3 || retained[0].Seq != 8 {
			t.Fatalf("expected sequences 8..10 retained, got %v", seqsOf(retained))
		}

		cases := []struct {
			after uint64
			want  []uint64
		}{
			{0, []uint64{8, 9, 10}}, // fell off the window entirely
			{3, []uint64{8, 9, 10}}, // still behind the oldest retained
			{7, []uint64{8, 9, 10}}, // exactly one behind
			{8, []uint64{9, 10}},    // mid-window
			{9, []uint64{10}},       // at the second-newest
			{10, nil},               // caught up
			{11, nil},               // ahead of the head (a stale cursor from a longer log)
			{1 << 40, nil},          // absurdly ahead
		}
		if len(cases) == 0 {
			t.Fatal("the pruned-cursor table is empty")
		}
		checked := 0
		for _, c := range cases {
			batch, next, _ := s.Since(b, noEpoch, c.after, 100)
			got := seqsOf(batch)
			if len(got) != len(c.want) {
				t.Fatalf("Since(after=%d) returned %v, want %v", c.after, got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("Since(after=%d) returned %v, want %v", c.after, got, c.want)
				}
			}
			if len(c.want) == 0 && next != c.after {
				t.Fatalf("Since(after=%d) with an empty batch moved the cursor to %d", c.after, next)
			}
			checked++
		}
		if checked != len(cases) {
			t.Fatalf("checked %d cursor positions, want %d", checked, len(cases))
		}
	})

	t.Run("SinceItselfPrunesOnAnIdleBus", func(t *testing.T) {
		// Append alone would mean the retention clock only advances when someone
		// sends, so an idle bus would hold a day-old stream open for ever.
		clock := newClock()
		s := store.New(store.Options{MaxAge: time.Hour, Now: clock.now})
		a := agentIDFor(t, "alpha")
		b := agentIDFor(t, "beta")

		mustAppend(t, s, mkMessage(t, a, true, nil, 1, clock.now(), "stale"))
		clock.advance(2 * time.Hour)

		if got := len(mustSince(t, s, b, 0, 100)); got != 0 {
			t.Fatalf("Since did not prune on an idle bus: %d retained", got)
		}
		if _, _, _, _, dropped := s.Stats(); dropped != 1 {
			t.Fatalf("dropped = %d after an idle prune, want 1", dropped)
		}
	})
}

func TestStoreDefaults(t *testing.T) {
	// The zero Options must yield the DOCUMENTED defaults (1 day / 1 GiB), not
	// zero-valued bounds that would drop everything on the first prune.
	s := store.New(store.Options{})
	a := agentIDFor(t, "alpha")
	b := agentIDFor(t, "beta")

	mustAppend(t, s, mkMessage(t, a, true, nil, 1, time.Now(), "recent"))
	if got := len(mustSince(t, s, b, 0, 10)); got != 1 {
		t.Fatalf("a fresh message was pruned by the default bounds: %d retained", got)
	}

	// Negative bounds fall back to the defaults too.
	s2 := store.New(store.Options{MaxAge: -time.Hour, MaxBytes: -1})
	mustAppend(t, s2, mkMessage(t, a, true, nil, 1, time.Now(), "recent"))
	if got := len(mustSince(t, s2, b, 0, 10)); got != 1 {
		t.Fatalf("negative bounds did not fall back to the defaults: %d retained", got)
	}
	if store.DefaultMaxAge != 24*time.Hour {
		t.Fatalf("DefaultMaxAge = %v, want 24h", store.DefaultMaxAge)
	}
	if store.DefaultMaxBytes != 1<<30 {
		t.Fatalf("DefaultMaxBytes = %d, want 1 GiB", store.DefaultMaxBytes)
	}
}

// ---------------------------------------------------------------------------
// The ENROLMENT EPOCH on the read path (the P0 from the 2026-08-02 audit).
//
// A message sent BEFORE an agent enrolled is never delivered to it. The store
// half of that is asserted here — the delivery half, and the proof that the
// long-poll WAKE honours it too, is TestEnrolmentEpoch in internal/hub.
// ---------------------------------------------------------------------------

func TestStoreEnrolmentEpoch(t *testing.T) {
	clock := newClock()
	s := store.New(store.Options{Now: clock.now})
	a := agentIDFor(t, "alpha")
	b := agentIDFor(t, "beta")

	sent := clock.now()
	mustAppend(t, s, mkMessage(t, a, true, nil, 1, sent, "a broadcast"))
	mustAppend(t, s, mkMessage(t, a, false, []string{b}, 2, sent, "a DM for beta"))

	cases := []struct {
		name       string
		enrolledAt time.Time
		want       []uint64
	}{
		{"EnrolledLongBefore", sent.Add(-time.Hour), []uint64{1, 2}},
		{"EnrolledOneNanosecondBefore", sent.Add(-time.Nanosecond), []uint64{1, 2}},
		// The boundary has exactly one spelling: VisibleTo rejects
		// SentAt.Before(enrolledAt), so a message sent AT the enrolment instant
		// is delivered.
		{"EnrolledAtTheSameInstant", sent, []uint64{1, 2}},
		{"EnrolledOneNanosecondAfter", sent.Add(time.Nanosecond), nil},
		{"EnrolledLongAfter", sent.Add(time.Hour), nil},
		// The zero epoch is the roster-less audit read and disables the check.
		{"ZeroEpoch", noEpoch, []uint64{1, 2}},
	}
	if len(cases) == 0 {
		t.Fatal("the enrolment-epoch table is empty")
	}
	checked := 0
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			batch, next, more := s.Since(b, c.enrolledAt, 0, 100)
			if got := seqsOf(batch); len(got) != len(c.want) {
				t.Fatalf("Since(beta, enrolledAt=%v) returned %v, want %v", c.enrolledAt, got, c.want)
			}
			for i := range c.want {
				if batch[i].Seq != c.want[i] {
					t.Fatalf("Since(beta, enrolledAt=%v) returned %v, want %v", c.enrolledAt, seqsOf(batch), c.want)
				}
			}
			if more {
				t.Fatalf("Since(beta, enrolledAt=%v) reported more with a limit of 100", c.enrolledAt)
			}
			if len(c.want) == 0 && next != 0 {
				t.Fatalf("an epoch-filtered empty batch moved the cursor to %d", next)
			}
			// HasVisibleAfter is the PARK predicate and must agree exactly, or a
			// long poll parks on messages its own read would then filter out.
			if got, want := s.HasVisibleAfter(b, c.enrolledAt, 0), len(c.want) > 0; got != want {
				t.Fatalf("HasVisibleAfter(beta, %v, 0) = %v but Since returned %d messages", c.enrolledAt, got, len(c.want))
			}
		})
		checked++
	}
	if checked != len(cases) {
		t.Fatalf("checked %d epochs, want %d", checked, len(cases))
	}

	// The messages are still RETAINED throughout: the epoch refuses DELIVERY, it
	// does not delete anything. Without this the test could pass on a store that
	// had simply lost them.
	if count, _, _, head, dropped := s.Stats(); count != 2 || head != 2 || dropped != 0 {
		t.Fatalf("Stats = (count %d, head %d, dropped %d), want (2, 2, 0) — the epoch must refuse delivery, not drop messages", count, head, dropped)
	}
}

// ---------------------------------------------------------------------------
// The BYTE-BOUNDED batch (store.MaxBatchBytes).
// ---------------------------------------------------------------------------

func TestStoreSinceByteBudget(t *testing.T) {
	// Bodies at store.MaxBodyBytes (64 KiB): 16 of them are exactly
	// MaxBatchBytes, so the byte budget binds long before the count limit of 100.
	const (
		bodyBytes = store.MaxBodyBytes
		n         = 20
		limit     = 100
	)
	wantPerBatch := int(store.MaxBatchBytes) / bodyBytes
	if wantPerBatch <= 0 || wantPerBatch >= n {
		t.Fatalf("test bug: a %d-byte budget over %d-byte bodies yields %d per batch, which does not bind before n=%d", store.MaxBatchBytes, bodyBytes, wantPerBatch, n)
	}

	clock := newClock()
	s := store.New(store.Options{Now: clock.now})
	a := agentIDFor(t, "alpha")
	b := agentIDFor(t, "beta")

	body := strings.Repeat("x", bodyBytes)
	for i := uint64(1); i <= n; i++ {
		mustAppend(t, s, mkMessage(t, a, true, nil, i, clock.now(), body))
	}

	t.Run("TheBatchIsCutByBytesNotByCount", func(t *testing.T) {
		batch, next, more := s.Since(b, noEpoch, 0, limit)
		if len(batch) != wantPerBatch {
			t.Fatalf("Since(limit=%d) returned %d messages, want %d — the byte budget did not bind", limit, len(batch), wantPerBatch)
		}
		if len(batch) >= limit {
			t.Fatalf("the batch of %d was cut by the COUNT limit of %d, so this proves nothing about bytes", len(batch), limit)
		}
		if !more {
			t.Fatal("a batch cut short by the byte budget did not report more")
		}
		var total int64
		for _, m := range batch {
			total += int64(m.Size())
		}
		if total > store.MaxBatchBytes {
			t.Fatalf("the batch carries %d bytes, over the %d budget", total, store.MaxBatchBytes)
		}
		if next != batch[len(batch)-1].Seq {
			t.Fatalf("cursor = %d, want the last delivered sequence %d", next, batch[len(batch)-1].Seq)
		}
	})

	t.Run("PagingTerminatesAndDeliversEachMessageExactlyOnce", func(t *testing.T) {
		seen := map[uint64]int{}
		after := uint64(0)
		rounds := 0
		for {
			batch, next, more := s.Since(b, noEpoch, after, limit)
			rounds++
			if rounds > n+2 {
				t.Fatalf("paging did not terminate after %d rounds", rounds)
			}
			if more && len(batch) == 0 {
				t.Fatal("more is true but the batch is empty — a polite pager would spin for ever here")
			}
			for _, m := range batch {
				seen[m.Seq]++
			}
			if next == after && len(batch) > 0 {
				t.Fatalf("a non-empty batch left the cursor at %d", after)
			}
			after = next
			if !more {
				break
			}
		}
		if rounds < 2 {
			t.Fatalf("paging finished in %d round(s), so the byte budget never cut a batch", rounds)
		}
		if len(seen) != n {
			t.Fatalf("paging delivered %d distinct messages over %d rounds, want %d", len(seen), rounds, n)
		}
		for seq, count := range seen {
			if count != 1 {
				t.Fatalf("sequence %d was delivered %d times across the pages, want exactly 1", seq, count)
			}
		}
	})

	t.Run("AMessageLargerThanTheWholeBudgetIsStillDelivered", func(t *testing.T) {
		// At least ONE message is always returned even if it alone exceeds the
		// budget, or a client that stops when more is false spins for ever on a
		// message it can never be handed.
		//
		// The body is built by hand rather than through NewMessage: nothing the
		// write path accepts is over MaxBodyBytes (64 KiB), so the only way to
		// reach this branch of Since's arithmetic is to append an oversized
		// record directly. It is the arithmetic under test, not the write path.
		clock2 := newClock()
		s2 := store.New(store.Options{Now: clock2.now})
		huge := mkMessage(t, a, true, nil, 1, clock2.now(), "placeholder")
		huge.Body = bytes.Repeat([]byte("h"), int(store.MaxBatchBytes)+1)
		huge.ContentSHA256 = store.ContentHash(huge.Body)
		mustAppend(t, s2, huge)
		mustAppend(t, s2, mkMessage(t, a, true, nil, 2, clock2.now(), "after the giant"))

		batch, next, more := s2.Since(b, noEpoch, 0, limit)
		if len(batch) != 1 {
			t.Fatalf("Since returned %d messages, want exactly the one oversized message", len(batch))
		}
		if batch[0].Seq != 1 {
			t.Fatalf("Since returned sequence %d, want the oversized message at 1", batch[0].Seq)
		}
		if int64(batch[0].Size()) <= store.MaxBatchBytes {
			t.Fatalf("the fixture message is %d bytes, not over the %d budget — this subtest proves nothing", batch[0].Size(), store.MaxBatchBytes)
		}
		if !more {
			t.Fatal("more is false although sequence 2 is still unread")
		}
		if next != 1 {
			t.Fatalf("cursor = %d, want 1", next)
		}
		// And the next page makes progress rather than repeating it.
		batch2, next2, more2 := s2.Since(b, noEpoch, next, limit)
		if len(batch2) != 1 || batch2[0].Seq != 2 {
			t.Fatalf("the second page returned %v, want just sequence 2", seqsOf(batch2))
		}
		if more2 {
			t.Fatal("the second page reported more with nothing left")
		}
		if next2 != 2 {
			t.Fatalf("cursor after the second page = %d, want 2", next2)
		}
	})
}

// TestStoreSinceReturnsDeepCopies pins that a caller cannot reach into the
// store's own slices through a returned batch. NewMessage copies carefully on
// the way IN; aliasing on the way OUT would defeat that entirely, and silently.
func TestStoreSinceReturnsDeepCopies(t *testing.T) {
	clock := newClock()
	s := store.New(store.Options{Now: clock.now})
	a := agentIDFor(t, "alpha")
	b := agentIDFor(t, "beta")

	mustAppend(t, s, mkMessage(t, a, false, []string{b}, 1, clock.now(), "original"))

	first, _, _ := s.Since(b, noEpoch, 0, 10)
	if len(first) != 1 {
		t.Fatalf("Since returned %d messages, want 1", len(first))
	}
	if len(first[0].Body) == 0 || len(first[0].Recipients) == 0 || len(first[0].BusPath) == 0 {
		t.Fatalf("the fixture message has an empty Body/Recipients/BusPath (%+v), so mutating them would prove nothing", first[0])
	}
	first[0].Body[0] = 'X'
	first[0].Recipients[0] = "clobbered"
	first[0].BusPath[0] = "clobbered"

	second, _, _ := s.Since(b, noEpoch, 0, 10)
	if len(second) != 1 {
		// Reaching here means the batch aliased the store's Recipients slice, so
		// clobbering it above RE-ADDRESSED a message the store still holds and
		// beta is no longer one of its recipients. That is the whole hazard: the
		// aliasing is silent until something writes through it.
		t.Fatalf("the second Since returned %d messages, want 1 — mutating a returned Recipients re-addressed the stored message", len(second))
	}
	if string(second[0].Body) != "original" {
		t.Fatalf("mutating a returned Body changed what the store returns next time: %q", second[0].Body)
	}
	if second[0].Recipients[0] != b {
		t.Fatalf("mutating a returned Recipients changed the store's copy: %v", second[0].Recipients)
	}
	if second[0].BusPath[0] != testBusID {
		t.Fatalf("mutating a returned BusPath changed the store's copy: %v", second[0].BusPath)
	}
	if second[0].ContentSHA256 != store.ContentHash([]byte("original")) {
		t.Fatalf("the store's ContentSHA256 no longer matches the body it still holds")
	}
}

// mustSince drains everything visible to agent, and fails on a truncated read
// so a caller can treat the result as the whole retained view.
//
// It reads with noEpoch on purpose: its callers are the RETENTION tests, whose
// fixtures move a synthetic clock decades around the messages' SentAt, so any
// non-zero epoch would silently entangle two independent filters. The epoch is
// covered separately — see TestStoreEnrolmentEpoch and TestStoreSince.
func mustSince(t *testing.T, s *store.Store, agent string, after uint64, limit int) []store.Message {
	t.Helper()
	batch, _, more := s.Since(agent, noEpoch, after, limit)
	if more {
		t.Fatalf("mustSince(%q, %d, %d) was cut short by the limit", agent, after, limit)
	}
	return batch
}

func seqsOf(msgs []store.Message) []uint64 {
	out := make([]uint64, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Seq)
	}
	return out
}
