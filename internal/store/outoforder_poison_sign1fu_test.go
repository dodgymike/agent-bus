package store_test

// SIGN-1-FU-OUTOFORDER-POISON — the UNIT-LEVEL half of the regression.
//
// SIGN-1 made a send a TWO-STEP: hub.Mint allocates and durably BURNS a sequence
// number so the client can sign it, and only then does the client send. Two
// agents can therefore hold reservations at the same time and spend them in ANY
// order — the reservation TTL is hub.MintTTL, fifteen minutes, so the window is
// not a race that is hard to hit, it is the ordinary shape of the protocol. A
// shorter TTL would narrow that window and close nothing.
//
// Store.Append still requires m.Seq > head. That rule was written when the
// sequence was allocated under writeMu IMMEDIATELY before the append, where
// "strictly increasing" and "never applied twice" were the same statement. They
// are not the same statement any more, and the difference is a P0: the hub has
// already completed the two-phase durable write and fsync before it calls
// Append, so a refusal here leaves the record COMMITTED ON DISK and rejected by
// the serving copy, which poisons the hub permanently (see the sibling test in
// internal/hub).
//
// The rule this file pins is therefore the NARROWED one:
//
//	a sequence that has never been applied may land LATE;
//	a sequence that HAS been applied may never land again;
//	and the head never rewinds (invariant 1 — ids are never reused and the
//	sequence never goes backwards).
//
// Everything below is a way of trying to break exactly that sentence. Nothing
// here relaxes the duplicate check: a genuine double-apply — a replay folding
// one entry twice, a write path reusing a number — is still the loud failure
// Append exists to catch.

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
)

// newOutOfOrderStore is a store on the injected clock. A local helper rather
// than a shared one so this file cannot change the fixtures the retention tests
// depend on.
func newOutOfOrderStore(clock *testClock) *store.Store {
	return store.New(store.Options{Now: clock.now})
}

// TestOutOfOrderMintSpendDoesNotPoison is the store-side proof for
// SIGN-1-FU-OUTOFORDER-POISON. See the file comment for the property.
func TestOutOfOrderMintSpendDoesNotPoison(t *testing.T) {
	// A LOWER sequence landing after a HIGHER one is the exact shape two agents
	// produce when they spend concurrent reservations out of order. It must be
	// ACCEPTED, and it must be accepted IN ORDER: Since binary-searches the
	// retained slice, so an insert appended at the end rather than in position
	// would be invisible to every reader whose cursor is above it.
	t.Run("ALowerSequenceMayLandAfterAHigherOne", func(t *testing.T) {
		clock := newClock()
		s := newOutOfOrderStore(clock)
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		// bob's reservation (2) is spent FIRST.
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 2, clock.now(), "spent-second-minted-first"))

		// alice's reservation (1) is spent SECOND. THIS IS THE ASSERTION THAT IS
		// RED AT HEAD: Append answers store.ErrOutOfOrder, and in the hub that
		// refusal arrives AFTER the record is already durable.
		late := mkMessage(t, alpha, true, nil, 1, clock.now(), "spent-second-minted-first-too")
		if err := s.Append(late); err != nil {
			t.Fatalf("appending sequence 1 after sequence 2 was REFUSED: %v\n"+
				"two agents holding concurrent mint reservations may spend them in any order, and the "+
				"record is already committed and fsynced by the time the serving copy sees it, so this "+
				"refusal poisons the bus (SIGN-1-FU-OUTOFORDER-POISON)", err)
		}

		// Visible to a reader whose cursor is BELOW the late insert, and in
		// ascending sequence order.
		got := seqsOf(mustSince(t, s, beta, 0, 16))
		if want := []uint64{1, 2}; !reflect.DeepEqual(got, want) {
			t.Fatalf("Since(after=0) = %v, want %v — the late sequence must be retained IN ORDER, not tacked onto the end", got, want)
		}
	})

	// The head is the HIGHEST sequence ever appended, and invariant 1 says it
	// never rewinds. Accepting a late lower number must not drag it backwards:
	// if it did, the very next number the bus handed out could be one it has
	// already used.
	t.Run("HeadNeverRewinds", func(t *testing.T) {
		clock := newClock()
		s := newOutOfOrderStore(clock)
		alpha := agentIDFor(t, "alpha")

		mustAppend(t, s, mkMessage(t, alpha, true, nil, 2, clock.now(), "two"))
		if got := s.Head(); got != 2 {
			t.Fatalf("Head() = %d after appending 2, want 2", got)
		}
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 1, clock.now(), "one"))
		if got := s.Head(); got != 2 {
			t.Fatalf("Head() = %d after a LATE append of sequence 1, want 2 — the head is the highest sequence "+
				"EVER appended and invariant 1 never rewinds it", got)
		}
	})

	// The table is the point: no arrival order may change what the bus serves.
	t.Run("EverySpendOrderYieldsTheSameStream", func(t *testing.T) {
		cases := []struct {
			name     string
			order    []uint64
			wantSeqs []uint64
			wantHead uint64
		}{
			{name: "in order", order: []uint64{1, 2, 3}, wantSeqs: []uint64{1, 2, 3}, wantHead: 3},
			{name: "one pair reversed", order: []uint64{2, 1, 3}, wantSeqs: []uint64{1, 2, 3}, wantHead: 3},
			{name: "fully reversed", order: []uint64{4, 3, 2, 1}, wantSeqs: []uint64{1, 2, 3, 4}, wantHead: 4},
			{name: "late arrival at the front", order: []uint64{3, 2, 1}, wantSeqs: []uint64{1, 2, 3}, wantHead: 3},
			// A GAP is legitimate and must not be compacted away: a mint whose
			// send never came burned its number for ever.
			{name: "with a burned-but-unspent gap", order: []uint64{5, 2}, wantSeqs: []uint64{2, 5}, wantHead: 5},
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				clock := newClock()
				s := newOutOfOrderStore(clock)
				alpha := agentIDFor(t, "alpha")
				beta := agentIDFor(t, "beta")

				for _, seq := range c.order {
					m := mkMessage(t, alpha, true, nil, seq, clock.now(), fmt.Sprintf("m%d", seq))
					if err := s.Append(m); err != nil {
						t.Fatalf("Append(seq=%d) in arrival order %v: %v", seq, c.order, err)
					}
				}
				if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, c.wantSeqs) {
					t.Fatalf("arrival order %v served %v, want %v", c.order, got, c.wantSeqs)
				}
				if got := s.Head(); got != c.wantHead {
					t.Fatalf("arrival order %v left Head() = %d, want %d", c.order, got, c.wantHead)
				}
			})
		}
	})

	// THE OTHER HALF OF THE NARROWING, and the one that must not be lost while
	// making the first half pass. A sequence that has ALREADY been applied is a
	// double-apply — a replay folding one entry twice, or a write path that
	// reused a number — and it stays LOUD.
	t.Run("ADuplicateSequenceIsStillRefused", func(t *testing.T) {
		clock := newClock()
		s := newOutOfOrderStore(clock)
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		mustAppend(t, s, mkMessage(t, alpha, true, nil, 2, clock.now(), "two"))
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 1, clock.now(), "one"))

		// Asserted on the SENTINEL, not merely on "some error". An err != nil
		// check passes under any implementation that fails for any reason at
		// all — including one that has stopped distinguishing a double-apply
		// from a malformed record — and this subtest is the only thing pinning
		// the half of the rule that did NOT relax.
		if err := s.Append(mkMessage(t, alpha, true, nil, 2, clock.now(), "two again")); !errors.Is(err, store.ErrDuplicateSequence) {
			t.Fatalf("re-appending sequence 2 returned %v, want store.ErrDuplicateSequence; a sequence that "+
				"has already been applied is a double-apply and must stay a loud, SPECIFIC error, whatever "+
				"the ordering rule becomes (invariant 1: ids are never reused)", err)
		}
		if err := s.Append(mkMessage(t, alpha, true, nil, 1, clock.now(), "one again")); !errors.Is(err, store.ErrDuplicateSequence) {
			t.Fatalf("re-appending sequence 1 — the LATE one — returned %v, want store.ErrDuplicateSequence; "+
				"relaxing the ordering rule must not relax the duplicate rule", err)
		}

		if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, []uint64{1, 2}) {
			t.Fatalf("after two refused duplicates the stream is %v, want [1 2] — a refused append must "+
				"leave the store untouched", got)
		}
		if count, _, _, head, _ := s.Stats(); count != 2 || head != 2 {
			t.Fatalf("after two refused duplicates Stats() reports count=%d head=%d, want count=2 head=2", count, head)
		}
	})

	// Sequence 0 is never allocated, in any order. This is the check that must
	// not be collateral damage of relaxing the comparison.
	t.Run("SequenceZeroIsStillInvalid", func(t *testing.T) {
		clock := newClock()
		s := newOutOfOrderStore(clock)
		alpha := agentIDFor(t, "alpha")

		mustAppend(t, s, mkMessage(t, alpha, true, nil, 2, clock.now(), "two"))

		// Built through the constructor and then MUTATED, because
		// store.NewMessage refuses sequence 0 outright (ids.MessageID) and so
		// cannot produce the value Append has to refuse. Every other field is
		// the one a valid message carries, so the ONLY thing wrong with what
		// Append sees is the sequence.
		zero := mkMessage(t, alpha, true, nil, 3, clock.now(), "zero")
		zero.Seq = 0
		err := s.Append(zero)
		if !errors.Is(err, store.ErrInvalidMessage) {
			t.Fatalf("Append(seq=0) = %v, want store.ErrInvalidMessage", err)
		}
		if got := s.Head(); got != 2 {
			t.Fatalf("Head() = %d after a refused sequence-0 append, want 2", got)
		}
	})

	// Retention accounts what is RETAINED, so a late insert has to be counted
	// like any other: a store that admitted the message but forgot its bytes
	// would drift its byte budget every time two agents raced.
	t.Run("StatsAccountTheLateInsert", func(t *testing.T) {
		clock := newClock()
		s := newOutOfOrderStore(clock)
		alpha := agentIDFor(t, "alpha")

		mustAppend(t, s, mkMessage(t, alpha, true, nil, 5, clock.now(), "12345")) // 5 bytes
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 3, clock.now(), "1"))     // 1 byte

		count, bytes, oldest, head, dropped := s.Stats()
		if count != 2 {
			t.Fatalf("Stats() count = %d, want 2 — the late insert is retained", count)
		}
		if bytes != 6 {
			t.Fatalf("Stats() bytes = %d, want 6 (5 + 1) — the late insert's body is accounted", bytes)
		}
		if oldest != 3 {
			t.Fatalf("Stats() oldest = %d, want 3 — the late insert is now the front of the retained window", oldest)
		}
		if head != 5 {
			t.Fatalf("Stats() head = %d, want 5", head)
		}
		if dropped != 0 {
			t.Fatalf("Stats() dropped = %d, want 0 — nothing aged out in this fixture", dropped)
		}
	})

	// SIGN-1-FU-OUTOFORDER-POISON's OTHER branch: a sequence that arrives after
	// retention has already dropped that position must be ACCEPTED (the record
	// is already committed and fsynced — an error here reopens a narrower form
	// of the same poison this task closes), must NOT be retained (retaining it
	// would resurrect a slot behind the window and silently re-admit an
	// already-pruned sequence), and must be LOUD about the discard (invariant 6:
	// the discard is sanctioned, silence is the defect).
	// The ORDINARY late insert is LOUD too, and this pins it.
	//
	// It is the branch that actually loses mail: the message is retained and
	// served to every cursor still behind it, but a reader that has already
	// passed the position never receives it and its parked long poll is never
	// woken. From that recipient's point of view the message was silently
	// dropped, which is the shape invariant 6 rates the defect — so the WARN is
	// the only thing making the loss observable to an operator, and an
	// unasserted log line is one a later edit deletes without any test noticing.
	t.Run("AnOrdinaryLateInsertIsLogged", func(t *testing.T) {
		clock := newClock()
		var logBuf bytes.Buffer
		s := store.New(store.Options{
			Now:    clock.now,
			Logger: logging.New(&logBuf, logging.LevelWarn),
		})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		mustAppend(t, s, mkMessage(t, alpha, true, nil, 2, clock.now(), "two"))
		if got := logBuf.String(); got != "" {
			t.Fatalf("an IN-ORDER append logged %q, want nothing; only an append BELOW the head loses a reader", got)
		}

		late := mkMessage(t, alpha, true, nil, 1, clock.now(), "one")
		mustAppend(t, s, late)

		// Asserted on the logfmt key=value pairs and the level, not on the prose,
		// so rewording the message does not break the test while deleting the
		// call does.
		got := logBuf.String()
		for _, want := range []string{"level=warn", "seq=1", "head=2", "message_id=" + late.ID} {
			if !strings.Contains(got, want) {
				t.Fatalf("the WARN for a late insert does not contain %q; an append below the head is invisible to "+
					"every reader already past it, and this line is the only thing that tells an operator so "+
					"(invariant 6). Log was:\n%s", want, got)
			}
		}

		// The message is still SERVED to a cursor behind it — the log line
		// reports a delivery gap, it does not describe a discard from the store.
		if seqs := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(seqs, []uint64{1, 2}) {
			t.Fatalf("after the logged late insert the stream is %v, want [1 2] — the WARN must not imply the message was dropped from the store", seqs)
		}
	})

	t.Run("ArrivalAtOrBelowThePrunedWatermarkIsAcceptedNotRetainedAndLogged", func(t *testing.T) {
		clock := newClock()
		var logBuf bytes.Buffer
		s := store.New(store.Options{
			MaxBytes: 20, // room for exactly two 10-byte bodies
			Now:      clock.now,
			Logger:   logging.New(&logBuf, logging.LevelWarn),
		})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		body := "0123456789"                                                   // 10 bytes
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 1, clock.now(), body)) // bytes=10
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 2, clock.now(), body)) // bytes=20
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 3, clock.now(), body)) // prunes 1; prunedHead=1
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 4, clock.now(), body)) // prunes 2; prunedHead=2

		if count, bytes, _, head, _ := s.Stats(); count != 2 || bytes != 20 || head != 4 {
			t.Fatalf("fixture setup: Stats = (count %d, bytes %d, head %d), want (2, 20, 4) — prunedHead should now be 2", count, bytes, head)
		}
		if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, []uint64{3, 4}) {
			t.Fatalf("fixture setup: retained = %v, want [3 4]", got)
		}

		cases := []uint64{1, 2} // strictly below, and exactly AT, the watermark (prunedHead=2)
		for _, lateSeq := range cases {
			lateSeq := lateSeq
			t.Run(fmt.Sprintf("seq=%d", lateSeq), func(t *testing.T) {
				logBuf.Reset()
				late := mkMessage(t, alpha, true, nil, lateSeq, clock.now(), fmt.Sprintf("late-%d", lateSeq))
				err := s.Append(late)
				if err != nil {
					t.Fatalf("Append(seq=%d), at or below prunedHead=2, returned an error (%v); it must return nil — the "+
						"record is already committed and fsynced by the time it reaches here, so an error here "+
						"reopens a narrower form of the very DoS SIGN-1-FU-OUTOFORDER-POISON closes", lateSeq, err)
				}

				// NOT RETAINED: Stats does not grow, and Since does not hand it out.
				if count, bytes, _, head, _ := s.Stats(); count != 2 || bytes != 20 || head != 4 {
					t.Fatalf("Append(seq=%d) changed Stats to (count %d, bytes %d, head %d), want (2, 20, 4) — a "+
						"sequence at or below prunedHead must not be retained", lateSeq, count, bytes, head)
				}
				if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, []uint64{3, 4}) {
					t.Fatalf("Append(seq=%d) is visible via Since: retained = %v, want [3 4] — retaining it would "+
						"resurrect a slot BEHIND the retention window", lateSeq, got)
				}

				// LOUD. Invariant 6 sanctions the discard, but silent discard is the
				// defect — this is the assertion that actually enforces that.
				logLine := logBuf.String()
				if !strings.Contains(logLine, "level=warn") {
					t.Fatalf("Append(seq=%d) did not log at WARN; got %q", lateSeq, logLine)
				}
				wantSeq := fmt.Sprintf("seq=%d", lateSeq)
				if !strings.Contains(logLine, wantSeq) {
					t.Fatalf("Append(seq=%d)'s WARN line does not mention the sequence (want %q); got %q", lateSeq, wantSeq, logLine)
				}
				if !strings.Contains(logLine, "pruned_head=2") {
					t.Fatalf("Append(seq=%d)'s WARN line does not mention pruned_head=2; got %q", lateSeq, logLine)
				}
			})
		}
	})

	// The new branch must not swallow the ORDINARY late-arrival case this task
	// exists to allow: a sequence ABOVE the watermark but below Head(), never
	// before applied, is still accepted AND retained.
	//
	// This one drives prunedHead up by AGE rather than by bytes: three uniform
	// 10-byte bodies always sum to 30, so any single MaxBytes bound that prunes
	// one of them away when three are present prunes another away the moment a
	// fourth lands, which proves nothing about the watermark specifically. Age
	// has no such symmetry — advancing the clock past exactly one message's
	// SentAt prunes exactly that one, independent of how many messages follow.
	t.Run("ArrivalAboveTheWatermarkIsStillAcceptedAndRetained", func(t *testing.T) {
		clock := newClock()
		s := store.New(store.Options{MaxAge: time.Hour, Now: clock.now})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		t0 := clock.now()
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 1, t0, "one"))
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 2, t0.Add(30*time.Minute), "two"))
		// A GAP: 3 is skipped, so it is still unapplied when it lands late.
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 4, t0.Add(31*time.Minute), "four"))

		// Advance the clock so seq 1's SentAt (t0) has aged out but seq 2 and 4's
		// have not: cutoff lands at t0+15m, strictly between t0 and t0+30m.
		clock.advance(time.Hour + 15*time.Minute)
		// Since prunes on read as well as on Append (an idle bus must age out
		// too), so this establishes prunedHead=1 without yet touching seq 3.
		if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, []uint64{2, 4}) {
			t.Fatalf("fixture setup: retained after the age-prune = %v, want [2 4] — prunedHead should now be 1", got)
		}
		if count, _, _, head, _ := s.Stats(); count != 2 || head != 4 {
			t.Fatalf("fixture setup: Stats = (count %d, head %d), want (2, 4)", count, head)
		}

		// seq 3 is above prunedHead(1) and below Head(4), and was never applied.
		mustAppend(t, s, mkMessage(t, alpha, true, nil, 3, clock.now(), "three, late"))

		if count, _, _, head, _ := s.Stats(); count != 3 || head != 4 {
			t.Fatalf("Append(seq=3) above the watermark left Stats = (count %d, head %d), want (3, 4) — "+
				"it must be RETAINED, not swallowed by the at-or-below-prunedHead branch", count, head)
		}
		if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, []uint64{2, 3, 4}) {
			t.Fatalf("Append(seq=3) above the watermark: retained = %v, want [2 3 4]", got)
		}
	})
}
