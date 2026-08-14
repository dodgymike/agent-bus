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
//
// # REVISED BY SIGN-1-FU-REORDER-WATERMARK — read this before "restoring" a case
//
// The rule above still holds in full. What changed underneath it is the ORDER
// the store keeps and the counter a cursor speaks: Message.Pos, the WAL commit
// index, is now the delivery position, and Message.Seq is identity only. Three
// expectations in this file were therefore INVERTED ON PURPOSE, and each says so
// at its own site:
//
//  1. A late, low sequence is served at the TAIL of the stream, not spliced into
//     sequence position. It is delivered to every reader — including the ones
//     that had already passed its sequence — which is the entire point of the
//     split, and the assertions that pinned ascending-Seq output would now be
//     pinning the suppression bug.
//  2. An ordinary late spend logs NOTHING. The old WARN existed because such a
//     message was lost to readers past it; it no longer is, so a log line would
//     be crying wolf on the normal shape of the protocol.
//  3. A sequence arriving after retention dropped that sequence's slot is
//     RETAINED, not discarded. It goes to the tail like any other late arrival,
//     above every live cursor, so refusing to retain it would be discarding an
//     acknowledged, genuinely deliverable message for no property gained.
//
// Positions are supplied explicitly here (mkMessageAt) because that is what the
// fixtures are about: ARRIVAL order and SEQUENCE order deliberately disagree.

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
	// ACCEPTED, and it must be accepted AT THE TAIL of the delivery order: Since
	// binary-searches by POSITION, so a message spliced into SEQUENCE position
	// would be invisible to every reader whose cursor is above it — which is the
	// suppression SIGN-1-FU-REORDER-WATERMARK removed.
	t.Run("ALowerSequenceMayLandAfterAHigherOne", func(t *testing.T) {
		clock := newClock()
		s := newOutOfOrderStore(clock)
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		// bob's reservation (2) is spent FIRST, so it commits at position 1.
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 2, 1, clock.now(), "minted-second-spent-first"))

		// alice's reservation (1) is spent SECOND, so it commits at position 2 —
		// a LOW sequence carrying a HIGH position.
		late := mkMessageAt(t, alpha, true, nil, 1, 2, clock.now(), "minted-first-spent-second")
		if err := s.Append(late); err != nil {
			t.Fatalf("appending sequence 1 after sequence 2 was REFUSED: %v\n"+
				"two agents holding concurrent mint reservations may spend them in any order, and the "+
				"record is already committed and fsynced by the time the serving copy sees it, so this "+
				"refusal poisons the bus (SIGN-1-FU-OUTOFORDER-POISON)", err)
		}

		// INVERTED BY SIGN-1-FU-REORDER-WATERMARK. This used to demand [1 2] —
		// ascending SEQUENCE — and that expectation was the suppression bug
		// written down: splicing the late arrival into sequence position puts it
		// below the cursor of every reader that has already been served seq 2.
		// The stream is DELIVERY order, so the late arrival is at the TAIL.
		got := seqsOf(mustSince(t, s, beta, 0, 16))
		if want := []uint64{2, 1}; !reflect.DeepEqual(got, want) {
			t.Fatalf("Since(after=0) served sequences %v, want %v — the stream is DELIVERY order (Message.Pos), "+
				"so a late spend is served LAST however low its sequence. If you are about to change this back to "+
				"[1 2], read the file header: sequence order is what lost the message", got, want)
		}

		// AND THE POINT OF IT ALL: a reader that had already consumed seq 2 — its
		// cursor is at position 1 — IS handed the late arrival.
		caughtUp, next, _ := s.Since(beta, noEpoch, 1, 16)
		if got := seqsOf(caughtUp); !reflect.DeepEqual(got, []uint64{1}) {
			t.Fatalf("a reader at the cursor it was given after seq 2 (position 1) was served %v, want [1] — "+
				"the late arrival must reach a reader that has already passed its SEQUENCE", got)
		}
		if next != 2 {
			t.Fatalf("next = %d after serving the late arrival, want its position 2", next)
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

		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 2, 1, clock.now(), "two"))
		if got := s.Head(); got != 2 {
			t.Fatalf("Head() = %d after appending 2, want 2", got)
		}
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 1, 2, clock.now(), "one"))
		if got := s.Head(); got != 2 {
			t.Fatalf("Head() = %d after a LATE append of sequence 1, want 2 — the head is the highest sequence "+
				"EVER appended and invariant 1 never rewinds it", got)
		}
		// The POSITION high-water mark is the other counter and it tracks arrival,
		// so it is 2 here for the opposite reason: the late arrival committed last.
		if got := s.PosHead(); got != 2 {
			t.Fatalf("PosHead() = %d, want 2 — the delivery position of the LAST record to commit", got)
		}
	})

	// EVERY SPEND ORDER IS SERVED IN THAT SAME ORDER, and no spend order may lose
	// a message.
	//
	// DELIBERATE REVERSAL (SIGN-1-FU-REORDER-WATERMARK). This subtest was called
	// EverySpendOrderYieldsTheSameStream and demanded ascending SEQUENCE output
	// whatever the arrival order — i.e. it asserted that the store re-sorts the
	// stream by identity. That is precisely what suppressed acknowledged
	// messages: re-sorting puts a late spend below cursors that have already
	// moved past its sequence, and nothing ever hands it to them.
	//
	// The property that replaces it is stronger where it matters and weaker only
	// where the old one was wrong: the stream is ARRIVAL order, every arrival
	// order delivers every message exactly once, and the head still never
	// rewinds. wantHead stays the MAXIMUM sequence, not the last one appended.
	t.Run("EverySpendOrderIsServedInThatSameArrivalOrder", func(t *testing.T) {
		cases := []struct {
			name     string
			order    []uint64
			wantHead uint64
		}{
			{name: "in order", order: []uint64{1, 2, 3}, wantHead: 3},
			{name: "one pair reversed", order: []uint64{2, 1, 3}, wantHead: 3},
			{name: "fully reversed", order: []uint64{4, 3, 2, 1}, wantHead: 4},
			{name: "late arrival at the front", order: []uint64{3, 2, 1}, wantHead: 3},
			// A GAP is legitimate and must not be compacted away: a mint whose
			// send never came burned its number for ever.
			{name: "with a burned-but-unspent gap", order: []uint64{5, 2}, wantHead: 5},
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				clock := newClock()
				s := newOutOfOrderStore(clock)
				alpha := agentIDFor(t, "alpha")
				beta := agentIDFor(t, "beta")

				for i, seq := range c.order {
					// The position IS the arrival index: this is a bus committing
					// records one after another, whatever sequence each carries.
					m := mkMessageAt(t, alpha, true, nil, seq, uint64(i+1), clock.now(), fmt.Sprintf("m%d", seq))
					if err := s.Append(m); err != nil {
						t.Fatalf("Append(seq=%d) in arrival order %v: %v", seq, c.order, err)
					}
				}
				if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, c.order) {
					t.Fatalf("arrival order %v served %v, want the SAME order — the stream is delivery order, "+
						"not sequence order (SIGN-1-FU-REORDER-WATERMARK)", c.order, got)
				}
				if got := s.Head(); got != c.wantHead {
					t.Fatalf("arrival order %v left Head() = %d, want %d", c.order, got, c.wantHead)
				}

				// NOBODY IS SKIPPED. Reading one message at a time from the cursor
				// the store itself hands back must reach every message, whatever
				// the arrival order — this is the property the reorder defect broke.
				var drained []uint64
				cursor := uint64(0)
				for round := 0; round <= len(c.order); round++ {
					batch, next, _ := s.Since(beta, noEpoch, cursor, 1)
					if len(batch) == 0 {
						break
					}
					drained = append(drained, batch[0].Seq)
					if next <= cursor {
						t.Fatalf("a non-empty batch left the cursor at %d", cursor)
					}
					cursor = next
				}
				if !reflect.DeepEqual(drained, c.order) {
					t.Fatalf("draining one at a time from arrival order %v yielded %v; every message must be "+
						"reachable from the cursor the store hands back", c.order, drained)
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

		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 2, 1, clock.now(), "two"))
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 1, 2, clock.now(), "one"))

		// Asserted on the SENTINEL, not merely on "some error". An err != nil
		// check passes under any implementation that fails for any reason at
		// all — including one that has stopped distinguishing a double-apply
		// from a malformed record — and this subtest is the only thing pinning
		// the half of the rule that did NOT relax.
		//
		// The re-appends carry FRESH, MONOTONE positions, so the only thing wrong
		// with either of them is the sequence. A duplicate that also reused its
		// position would be refused for two reasons at once and would stop
		// proving which one fired.
		if err := s.Append(mkMessageAt(t, alpha, true, nil, 2, 3, clock.now(), "two again")); !errors.Is(err, store.ErrDuplicateSequence) {
			t.Fatalf("re-appending sequence 2 returned %v, want store.ErrDuplicateSequence; a sequence that "+
				"has already been applied is a double-apply and must stay a loud, SPECIFIC error, whatever "+
				"the ordering rule becomes (invariant 1: ids are never reused)", err)
		}
		if err := s.Append(mkMessageAt(t, alpha, true, nil, 1, 4, clock.now(), "one again")); !errors.Is(err, store.ErrDuplicateSequence) {
			t.Fatalf("re-appending sequence 1 — the LATE one — returned %v, want store.ErrDuplicateSequence; "+
				"relaxing the ordering rule must not relax the duplicate rule", err)
		}

		if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, []uint64{2, 1}) {
			t.Fatalf("after two refused duplicates the stream is %v, want [2 1] (delivery order) — a refused "+
				"append must leave the store untouched", got)
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

		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 2, 1, clock.now(), "two"))

		// Built through the constructor and then MUTATED, because
		// store.NewMessage refuses sequence 0 outright (ids.MessageID) and so
		// cannot produce the value Append has to refuse. Every other field is
		// the one a valid message carries, so the ONLY thing wrong with what
		// Append sees is the sequence.
		zero := mkMessageAt(t, alpha, true, nil, 3, 2, clock.now(), "zero")
		zero.Seq = 0
		err := s.Append(zero)
		if !errors.Is(err, store.ErrInvalidMessage) {
			t.Fatalf("Append(seq=0) = %v, want store.ErrInvalidMessage", err)
		}
		if got := s.Head(); got != 2 {
			t.Fatalf("Head() = %d after a refused sequence-0 append, want 2", got)
		}
	})

	// POSITION zero is the sibling refusal, and it is not decoration: 0 is the
	// reserved "I have seen nothing" cursor, so a message stamped 0 would sit
	// below every cursor in existence and be re-served on every poll for ever.
	// It can only be produced by a caller that forgot to stamp the WAL commit
	// index (SIGN-1-FU-REORDER-WATERMARK).
	t.Run("PositionZeroIsInvalid", func(t *testing.T) {
		clock := newClock()
		s := newOutOfOrderStore(clock)
		alpha := agentIDFor(t, "alpha")

		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 2, 1, clock.now(), "two"))

		unstamped := mkMessageAt(t, alpha, true, nil, 3, 0, clock.now(), "unstamped")
		if err := s.Append(unstamped); !errors.Is(err, store.ErrInvalidMessage) {
			t.Fatalf("Append(pos=0) = %v, want store.ErrInvalidMessage", err)
		}
		if count, _, _, head, _ := s.Stats(); count != 1 || head != 2 {
			t.Fatalf("after a refused position-0 append Stats = (count %d, head %d), want (1, 2)", count, head)
		}
		if got := s.PosHead(); got != 1 {
			t.Fatalf("PosHead() = %d after a refused position-0 append, want 1", got)
		}
	})

	// Retention accounts what is RETAINED, so a late insert has to be counted
	// like any other: a store that admitted the message but forgot its bytes
	// would drift its byte budget every time two agents raced.
	t.Run("StatsAccountTheLateInsert", func(t *testing.T) {
		clock := newClock()
		s := newOutOfOrderStore(clock)
		alpha := agentIDFor(t, "alpha")

		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 5, 1, clock.now(), "12345")) // 5 bytes
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 3, 2, clock.now(), "1"))     // 1 byte

		count, bytes, oldest, head, dropped := s.Stats()
		if count != 2 {
			t.Fatalf("Stats() count = %d, want 2 — the late insert is retained", count)
		}
		if bytes != 6 {
			t.Fatalf("Stats() bytes = %d, want 6 (5 + 1) — the late insert's body is accounted", bytes)
		}
		// INVERTED: oldest is the sequence of the message at the FRONT OF THE
		// DELIVERY ORDER, which is the one that committed first (seq 5), not the
		// lowest sequence. The late arrival goes to the tail.
		if oldest != 5 {
			t.Fatalf("Stats() oldest = %d, want 5 — the front of the retained window is the FIRST record to "+
				"commit, and the late spend is at the tail (SIGN-1-FU-REORDER-WATERMARK)", oldest)
		}
		if head != 5 {
			t.Fatalf("Stats() head = %d, want 5", head)
		}
		if dropped != 0 {
			t.Fatalf("Stats() dropped = %d, want 0 — nothing aged out in this fixture", dropped)
		}
	})

	// INVERTED BY SIGN-1-FU-REORDER-WATERMARK. This subtest was
	// AnOrdinaryLateInsertIsLogged and it pinned a WARN emitted whenever a
	// sequence landed below the head.
	//
	// That line existed because such a message WAS lost — spliced into sequence
	// position, below the cursor of every reader that had passed it, never
	// delivered and never woken — so the log was the only trace of the loss. The
	// message is no longer lost: it lands at the tail of the delivery order,
	// above every live cursor, and reaches everyone. A WARN on the ordinary
	// two-agent spend would now be crying wolf on the protocol's normal shape,
	// once per message, on a bus whose operators would rightly learn to ignore
	// it — which is how a real invariant-6 line gets ignored too.
	//
	// So the assertion is inverted: the ordinary late spend is SILENT and
	// DELIVERED. The loud path that remains is the non-monotone POSITION, which
	// no client can reach; it has its own subtest below.
	t.Run("AnOrdinaryLateSpendIsSilentAndDelivered", func(t *testing.T) {
		clock := newClock()
		var logBuf bytes.Buffer
		s := store.New(store.Options{
			Now:    clock.now,
			Logger: logging.New(&logBuf, logging.LevelWarn),
		})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 2, 1, clock.now(), "two"))
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 1, 2, clock.now(), "one"))

		if got := logBuf.String(); got != "" {
			t.Fatalf("a late SPEND (low sequence, high position) logged %q, want nothing — it is delivered to "+
				"every reader, so there is nothing to report and a line here would train operators to ignore "+
				"the one that matters (SIGN-1-FU-REORDER-WATERMARK)", got)
		}
		if got := s.NonMonotonicPositions(); got != 0 {
			t.Fatalf("NonMonotonicPositions() = %d after two monotone appends, want 0", got)
		}
		if seqs := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(seqs, []uint64{2, 1}) {
			t.Fatalf("after the late spend the stream is %v, want [2 1] (delivery order)", seqs)
		}
	})

	// THE ONE LOUD PATH LEFT: a delivery position that is NOT above every
	// position already applied. It cannot be reached by any client — positions
	// are WAL commit indices minted under the hub's write lock — so it means the
	// durable write and the serving-copy append have stopped being serialised.
	//
	// It must RETAIN the message and RETURN NIL (the record is already committed
	// and fsynced; an error orphans it and halts the bus, which is the P0
	// SIGN-1-FU-OUTOFORDER-POISON fixed), and it must be LOUD and COUNTED —
	// invariant 6 sanctions the discard, never the silence.
	t.Run("ANonMonotonicPositionIsRetainedLoggedAndCounted", func(t *testing.T) {
		clock := newClock()
		var logBuf bytes.Buffer
		s := store.New(store.Options{
			Now:    clock.now,
			Logger: logging.New(&logBuf, logging.LevelWarn),
		})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 1, 10, clock.now(), "first"))
		logBuf.Reset()

		// Position 4 is BELOW the position already applied. Nothing legitimate
		// produces this.
		bad := mkMessageAt(t, alpha, true, nil, 2, 4, clock.now(), "out of order")
		if err := s.Append(bad); err != nil {
			t.Fatalf("Append with a non-monotone position returned %v; it must return nil — the record is "+
				"already committed and fsynced, so an error orphans it on disk and poisons the hub", err)
		}

		// RETAINED, and in position order so the binary search stays honest.
		if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, []uint64{2, 1}) {
			t.Fatalf("after a non-monotone append the stream is %v, want [2 1] — the message is retained and "+
				"ordered by position (4 then 10); dropping it would be the silent suppression this task removes", got)
		}
		if got := s.NonMonotonicPositions(); got != 1 {
			t.Fatalf("NonMonotonicPositions() = %d, want 1 — the condition must be observable, not log-only", got)
		}
		if got := s.PosHead(); got != 10 {
			t.Fatalf("PosHead() = %d, want 10 — the position high-water mark never rewinds", got)
		}

		// LOUD, and asserted on the logfmt pairs rather than the prose so that
		// rewording does not break the test while deleting the call does.
		logLine := logBuf.String()
		for _, want := range []string{"level=error", "pos=4", "pos_head=10", "seq=2", "message_id=" + bad.ID} {
			if !strings.Contains(logLine, want) {
				t.Fatalf("the non-monotone-position log does not contain %q; this line is the ONLY signal that the "+
					"write path's locking has stopped holding (invariant 6). Log was:\n%s", want, logLine)
			}
		}
	})

	// INVERTED BY SIGN-1-FU-REORDER-WATERMARK. This was
	// ArrivalAtOrBelowThePrunedWatermarkIsAcceptedNotRetainedAndLogged, and it
	// pinned a REFUSAL to retain a sequence that arrived after retention had
	// dropped that sequence's slot.
	//
	// Under delivery ordering there is no such slot to resurrect: the arrival
	// takes the NEXT position, lands at the tail above every live cursor, and is
	// genuinely deliverable to everybody. Refusing to retain it would discard an
	// acknowledged message for no property gained — which is exactly the
	// suppression this task exists to kill, and it also removes the prune-race
	// the security gate found, structurally rather than by narrowing a window.
	t.Run("ArrivalAfterItsSequenceWasPrunedIsRetainedAndDelivered", func(t *testing.T) {
		clock := newClock()
		var logBuf bytes.Buffer
		s := store.New(store.Options{
			MaxBytes: 20, // room for exactly two 10-byte bodies
			Now:      clock.now,
			Logger:   logging.New(&logBuf, logging.LevelWarn),
		})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		body := "0123456789"                                                        // 10 bytes
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 3, 1, clock.now(), body)) // bytes=10
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 4, 2, clock.now(), body)) // bytes=20
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 5, 3, clock.now(), body)) // prunes pos 1
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 6, 4, clock.now(), body)) // prunes pos 2

		if count, bytes, _, head, _ := s.Stats(); count != 2 || bytes != 20 || head != 6 {
			t.Fatalf("fixture setup: Stats = (count %d, bytes %d, head %d), want (2, 20, 6)", count, bytes, head)
		}
		if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, []uint64{5, 6}) {
			t.Fatalf("fixture setup: retained = %v, want [5 6]", got)
		}

		// A reader that has kept up sits at the position of seq 6.
		_, cursor, _ := s.Since(beta, noEpoch, 0, 16)
		if cursor != 4 {
			t.Fatalf("fixture setup: the caught-up reader's cursor is %d, want 4", cursor)
		}

		// Sequences 1 and 2 were minted long ago and are only spent now — after
		// everything around them has already been pruned.
		for i, lateSeq := range []uint64{1, 2} {
			lateSeq, pos := lateSeq, uint64(5+i)
			t.Run(fmt.Sprintf("seq=%d", lateSeq), func(t *testing.T) {
				logBuf.Reset()
				late := mkMessageAt(t, alpha, true, nil, lateSeq, pos, clock.now(), fmt.Sprintf("late-%d", lateSeq))
				if err := s.Append(late); err != nil {
					t.Fatalf("Append(seq=%d) after its sequence's slot was pruned returned %v; it must return nil — "+
						"the record is already committed and fsynced", lateSeq, err)
				}

				// RETAINED and DELIVERED to the reader that had already passed
				// every sequence in the fixture. This is the whole inversion.
				batch, next, _ := s.Since(beta, noEpoch, cursor, 16)
				if got := seqsOf(batch); !reflect.DeepEqual(got, []uint64{lateSeq}) {
					t.Fatalf("the caught-up reader at cursor %d was served %v, want [%d] — a late arrival takes the "+
						"NEXT position, so it is above every live cursor and must reach every reader "+
						"(SIGN-1-FU-REORDER-WATERMARK)", cursor, got, lateSeq)
				}
				if next != pos {
					t.Fatalf("next = %d after serving the late arrival, want its position %d", next, pos)
				}
				cursor = next

				// It is a NORMAL event, so nothing is logged and nothing is counted
				// as a fault.
				if got := logBuf.String(); got != "" {
					t.Fatalf("Append(seq=%d) logged %q, want nothing — it is an ordinary late spend, not damage", lateSeq, got)
				}
				if got := s.NonMonotonicPositions(); got != 0 {
					t.Fatalf("NonMonotonicPositions() = %d, want 0 — the position was monotone", got)
				}
			})
		}
	})

	// The ordinary late arrival that lands while its neighbours are still
	// retained is accepted and retained too. It drives the retention window by
	// AGE rather than by bytes: three uniform 10-byte bodies always sum to 30, so
	// any single MaxBytes bound that prunes one of them away when three are
	// present prunes another the moment a fourth lands, which proves nothing.
	// Age has no such symmetry — advancing the clock past exactly one message's
	// SentAt prunes exactly that one, independent of how many messages follow.
	t.Run("ALateArrivalWithinTheWindowIsAcceptedAndRetained", func(t *testing.T) {
		clock := newClock()
		s := store.New(store.Options{MaxAge: time.Hour, Now: clock.now})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		t0 := clock.now()
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 1, 1, t0, "one"))
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 2, 2, t0.Add(30*time.Minute), "two"))
		// A GAP: 3 is skipped, so it is still unapplied when it lands late.
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 4, 3, t0.Add(31*time.Minute), "four"))

		// Advance the clock so seq 1's SentAt (t0) has aged out but seq 2 and 4's
		// have not: cutoff lands at t0+15m, strictly between t0 and t0+30m.
		clock.advance(time.Hour + 15*time.Minute)
		// Since prunes on read as well as on Append (an idle bus must age out too).
		if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, []uint64{2, 4}) {
			t.Fatalf("fixture setup: retained after the age-prune = %v, want [2 4]", got)
		}
		if count, _, _, head, _ := s.Stats(); count != 2 || head != 4 {
			t.Fatalf("fixture setup: Stats = (count %d, head %d), want (2, 4)", count, head)
		}

		// seq 3 was minted before seq 4 and is spent now, at the next position.
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 3, 4, clock.now(), "three, late"))

		if count, _, _, head, _ := s.Stats(); count != 3 || head != 4 {
			t.Fatalf("Append(seq=3) left Stats = (count %d, head %d), want (3, 4) — a late spend is RETAINED", count, head)
		}
		if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, []uint64{2, 4, 3}) {
			t.Fatalf("after the late spend: retained = %v, want [2 4 3] (delivery order)", got)
		}
	})
}
