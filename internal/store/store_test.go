// Package store_test exercises the serving copy through its exported surface.
package store_test

import (
	"errors"
	"fmt"
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
	m, err := store.NewMessage(testBusID, sender, broadcast, recipients, seq, sentAt, []byte(body), fmt.Sprintf("key-%d", seq))
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
		cases := []struct {
			agent string
			want  []uint64
		}{
			{a, []uint64{3}},          // its own sends are excluded
			{b, []uint64{1, 2, 5}},    // broadcasts from a, plus its DM
			{g, []uint64{1, 3, 4, 5}}, // both broadcasts plus its DM
			{"", nil},                 // an empty principal sees nothing
			{agentIDFor(t, "delta"), []uint64{1, 3, 5}}, // a later arrival reads back through the window
		}
		if len(cases) == 0 {
			t.Fatal("the visibility table is empty")
		}
		checked := 0
		for _, c := range cases {
			batch, next, more := s.Since(c.agent, 0, 100)
			if len(batch) != len(c.want) {
				t.Fatalf("Since(%q, 0, 100) returned %d messages, want %d", c.agent, len(batch), len(c.want))
			}
			for i, m := range batch {
				if m.Seq != c.want[i] {
					t.Fatalf("Since(%q)[%d] has sequence %d, want %d", c.agent, i, m.Seq, c.want[i])
				}
			}
			if more {
				t.Fatalf("Since(%q, 0, 100) reported more with a limit of 100", c.agent)
			}
			if len(c.want) == 0 {
				if next != 0 {
					t.Fatalf("Since(%q) with an empty batch moved the cursor to %d", c.agent, next)
				}
			} else if next != c.want[len(c.want)-1] {
				t.Fatalf("Since(%q) returned cursor %d, want %d", c.agent, next, c.want[len(c.want)-1])
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
			batch, next, more := s.Since(g, after, 2)
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
			batch, next, more := s.Since(g, 2, limit)
			if len(batch) != 0 || more {
				t.Fatalf("Since(limit=%d) returned %d messages (more=%v), want none", limit, len(batch), more)
			}
			if next != 2 {
				t.Fatalf("Since(limit=%d) moved the cursor from 2 to %d", limit, next)
			}
		}
	})

	t.Run("HasVisibleAfter", func(t *testing.T) {
		cases := []struct {
			agent string
			after uint64
			want  bool
		}{
			{g, 0, true},
			{g, 4, true},  // 5 is still ahead
			{g, 5, false}, // caught up
			{g, 99, false},
			{a, 3, false}, // everything after 3 is a's own
			{a, 0, true},
			{"", 0, false},
			{agentIDFor(t, "nobody"), 0, true}, // broadcasts are visible to anyone
		}
		if len(cases) == 0 {
			t.Fatal("the HasVisibleAfter table is empty")
		}
		for _, c := range cases {
			if got := s.HasVisibleAfter(c.agent, c.after); got != c.want {
				t.Fatalf("HasVisibleAfter(%q, %d) = %v, want %v", c.agent, c.after, got, c.want)
			}
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
			batch, next, _ := s.Since(b, c.after, 100)
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

// mustSince drains everything visible to agent, and fails on a truncated read
// so a caller can treat the result as the whole retained view.
func mustSince(t *testing.T, s *store.Store, agent string, after uint64, limit int) []store.Message {
	t.Helper()
	batch, _, more := s.Since(agent, after, limit)
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
