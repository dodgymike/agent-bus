package store_test

// SIGN-1-FU-REORDER-WATERMARK — THE UNIT-LEVEL SHAPE OF THE FIX.
//
// # The defect this file used to measure
//
// Since SIGN-1 a send is a TWO-STEP: hub.Mint durably burns a sequence so the
// client can sign it, and the send follows, up to hub.MintTTL (fifteen minutes)
// later. Two agents holding reservations may spend them in either order, so a
// message can be COMMITTED, FSYNCED AND ACKED at a sequence BELOW a reader's
// cursor. While a cursor was a SEQUENCE and the serving copy was kept in
// sequence order, Since binary-searched strictly after the cursor and never
// reached that message — not on that poll, not on any later one. The sender
// chose WHEN to spend, so it chose the victim: a targeted suppression and
// false-ack primitive rather than a race.
//
// # What replaced it: the seq/pos SPLIT, not a watermark
//
//   - Message.Seq is unchanged — the server-minted, client-signed IDENTITY,
//     handed out at RESERVATION time and therefore NOT monotone in arrival order.
//   - Message.Pos is the DELIVERY POSITION: the WAL commit index of the record
//     that made the message durable, monotone by construction. Cursors, Since,
//     HasVisibleAfter and hub.notify all speak it.
//
// A late arrival with a LOW Seq therefore gets a HIGH Pos, lands ABOVE every
// live cursor, and is served to every reader. The watermark this task is named
// after ("no sequence <= W can still arrive") was REFUTED by execution and is
// quantified below in TestReorderWatermarkTheGapCanBeArbitrarilyWide: one
// unspent reservation would have withheld 200 already-acknowledged messages from
// every reader on the bus.
//
// # THIS FILE WAS A FALSE GREEN, and the guard against that is now explicit
//
// Its first version passed, and the pass was worthless. Its fixture used
// store_test.mkMessage, which stamps Pos == Seq — so an out-of-order fixture
// handed Append a position BELOW the one already applied, drove the
// NON-MONOTONE-POSITION FAULT BRANCH (log at ERROR, retain via ordered insert,
// return nil), and then asserted the old suppressing shape that branch still
// produces. It measured the server-bug path while claiming to measure the
// protocol's ordinary one.
//
// So two rules hold here for ever:
//
//	1. Fixtures assign Pos in ARRIVAL ORDER, independent of Seq — mkMessageAt,
//	   never mkMessage — because arrival order disagreeing with sequence order is
//	   the entire subject.
//	2. Every test on the NORMAL path asserts NonMonotonicPositions() == 0. That
//	   single assertion is what makes a fixture that has silently slipped onto the
//	   fault branch fail LOUDLY instead of passing for the wrong reason.
//
// The fault branch keeps its own coverage — see
// TestReorderWatermarkANonMonotonicPositionIsTheFaultBranchNotTheNormalPath —
// because it must retain and stay up rather than drop silently.
//
// # Invariants read IN FULL before writing this
//
// Invariant 1 (ids never reused, the sequence never rewinds): the head is the
// highest SEQUENCE ever appended and a late lower number must not drag it back;
// PosHead is the other counter and only ever grows too. Invariant 4 (nothing
// acknowledged before durable): the send IS acked and the record IS durable, so
// a store that refused to serve it would be losing acknowledged mail, not
// protecting anything. Invariant 5 (memory serves, disk is truth): the position
// is the WAL commit index precisely so a restart reproduces it exactly.
// Invariant 6 (loud discards): the one remaining fault path logs at ERROR and is
// counted; silence is the defect. Invariant 10 (idempotency): delivering the
// straggler must not re-deliver anything else, which the cursor assertions below
// pin.

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

// reorderFixture is the minimal reproduction, built the way the hub builds it:
// POSITIONS ARE ARRIVAL ORDER and sequences are identities handed out earlier
// and spent whenever their holders please.
//
//	pos 1 — seq 3   bob's first reservation, spent first
//	pos 2 — seq 4   bob's second reservation, spent second
//	pos 3 — seq 1   ALICE's reservation, minted FIRST and spent LAST
//
// Sequence 2 is a burned-but-never-spent mint. It is legitimate — a mint whose
// send never came burns its number for ever (invariant 1) — and it is in the
// fixture so nothing here can quietly assume the sequence space is dense.
//
// The distance between alice's SEQUENCE (1) and her POSITION (3) is the point.
// A reader at cursor 2 is STRICTLY ABOVE her sequence and STRICTLY BELOW her
// position: under the old sequence-cursor design that reader was suppressed for
// ever, and under the split it is served.
//
// It returns the store and the reading agent.
func reorderFixture(t *testing.T) (*store.Store, string) {
	t.Helper()
	clock := newClock()
	s := newOutOfOrderStore(clock)
	alpha := agentIDFor(t, "alpha")
	beta := agentIDFor(t, "beta")

	mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 3, 1, clock.now(), "bob, spent FIRST"))
	mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 4, 2, clock.now(), "bob, spent SECOND"))
	mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 1, 3, clock.now(), "alice, minted FIRST, spent LAST"))

	assertNormalPath(t, s)
	return s, beta
}

// assertNormalPath is the guard that stops the FALSE GREEN this file was.
//
// A fixture that stamps Pos == Seq (store_test.mkMessage) hands Append a
// position below the one already applied whenever it appends sequences out of
// order. Append retains that message and returns nil — correctly, because the
// record is already durable — so the ONLY externally visible difference between
// "this test exercised the protocol's ordinary late spend" and "this test
// exercised the server-bug branch" is this counter. Every test on the normal
// path calls this.
func assertNormalPath(t *testing.T, s *store.Store) {
	t.Helper()
	if got := s.NonMonotonicPositions(); got != 0 {
		t.Fatalf("NonMonotonicPositions() = %d, want 0. THIS FIXTURE IS NOT ON THE NORMAL PATH: it appended a "+
			"delivery position that was not above every position already applied, so Append took the SERVER-BUG "+
			"branch and every assertion after this point is measuring that branch instead of the ordinary "+
			"out-of-order spend. Almost always the cause is building fixtures with mkMessage (which stamps "+
			"Pos == Seq) rather than mkMessageAt (which takes the ARRIVAL position). See this file's header — "+
			"the first version of it passed for exactly this reason (SIGN-1-FU-REORDER-WATERMARK)", got)
	}
}

// TestReorderWatermarkSinceShape is the table: for every cursor POSITION, what
// Since hands back, what it reports as `next`, and what HasVisibleAfter — the
// cheap predicate the long-poll park path consults — says about the same point.
//
// The cursor-2 row is the fix. Everything else is the control that proves the
// late arrival really is stored, really is retained, and is served to readers on
// both sides of its sequence — so the cursor-2 row cannot be explained away.
func TestReorderWatermarkSinceShape(t *testing.T) {
	s, beta := reorderFixture(t)

	// Premise guard. If the fixture ever stopped retaining all three messages the
	// table below would still pass while measuring nothing.
	count, _, oldest, head, dropped := s.Stats()
	if count != 3 || oldest != 3 || head != 4 || dropped != 0 {
		t.Fatalf("PREMISE BROKEN: Stats = (count %d, oldest %d, head %d, dropped %d), want (3, 3, 4, 0). oldest is "+
			"the sequence at the FRONT OF THE DELIVERY ORDER — the first record to commit — not the lowest "+
			"sequence; head is the highest sequence ever appended and never rewinds (invariant 1)",
			count, oldest, head, dropped)
	}
	if got := s.PosHead(); got != 3 {
		t.Fatalf("PREMISE BROKEN: PosHead() = %d, want 3 — three records committed, at positions 1, 2 and 3", got)
	}

	cases := []struct {
		name string
		// after is the reader's cursor, which is a DELIVERY POSITION.
		after uint64
		// wantSeqs is what Since hands back, identified by SEQUENCE so a failure
		// names which message went missing.
		wantSeqs []uint64
		// wantNext is the cursor the reader is told to resume from — a position.
		wantNext uint64
		// wantHasVisible is store.HasVisibleAfter, the predicate hub.Wait consults
		// before parking. It is in the table because a fix that repaired Since
		// alone would leave a parked poll deciding to keep sleeping on a message
		// its own next read would return.
		wantHasVisible bool
		// why records what this row establishes.
		why string
	}{
		{
			name:  "cursor 0 — a reader who has seen nothing gets ALL THREE, in DELIVERY order",
			after: 0, wantSeqs: []uint64{3, 4, 1}, wantNext: 3, wantHasVisible: true,
			why: "the stream is ordered by Message.Pos, so alice's late spend is served LAST however low its " +
				"sequence; an ascending-sequence expectation here would be the suppression bug written down",
		},
		{
			name:  "cursor 1 — a reader that has consumed bob's first spend",
			after: 1, wantSeqs: []uint64{4, 1}, wantNext: 3, wantHasVisible: true,
			why: "the boundary is STRICTLY-AFTER the position, and everything above it is still to come",
		},
		{
			name:  "cursor 2 — THE FIX: a reader ABOVE alice's SEQUENCE and below her POSITION is SERVED her message",
			after: 2, wantSeqs: []uint64{1}, wantNext: 3, wantHasVisible: true,
			why: "this cursor (2) is strictly above alice's sequence (1), which is exactly the reader the old " +
				"sequence-cursor design suppressed for ever. Her message committed LAST, so it carries position 3 " +
				"and lands ABOVE this cursor; it is delivered, and the cursor advances past it",
		},
		{
			name:  "cursor 3 — a reader that has seen everything",
			after: 3, wantSeqs: nil, wantNext: 3, wantHasVisible: false,
			why: "the head of the delivery order really is the head; nothing is re-served, so closing the gap did " +
				"not turn into an unbounded replay (invariant 10)",
		},
		{
			name:  "cursor 9 — above every position ever assigned",
			after: 9, wantSeqs: nil, wantNext: 9, wantHasVisible: false,
			why: "the empty-batch contract leaves the cursor untouched (POLL-1), whatever the cursor was",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			batch, next, more := s.Since(beta, noEpoch, c.after, 16)
			if got := seqsOf(batch); !reflect.DeepEqual(nonEmpty(got), nonEmpty(c.wantSeqs)) {
				t.Fatalf("Since(after=%d) served sequences %v, want %v.\nPROPERTY: %s.\n"+
					"A cursor is a DELIVERY POSITION (store.Message.Pos), never a sequence "+
					"(SIGN-1-FU-REORDER-WATERMARK)", c.after, got, c.wantSeqs, c.why)
			}
			if next != c.wantNext {
				t.Fatalf("Since(after=%d) reported next = %d, want %d — %s", c.after, next, c.wantNext, c.why)
			}
			if more {
				t.Fatalf("Since(after=%d) reported more = true on a 3-message store read with limit 16", c.after)
			}
			if got := s.HasVisibleAfter(beta, noEpoch, c.after); got != c.wantHasVisible {
				t.Fatalf("HasVisibleAfter(after=%d) = %v, want %v — this is the predicate hub.Wait consults before "+
					"parking, so it must agree with Since or a poll parks on a message it would have been served "+
					"(or refuses to park when it would be served nothing)", c.after, got, c.wantHasVisible)
			}
		})
	}

	// Reads do not append, so this cannot have moved — which is the point: it
	// re-states, after the table has run, that every row above was measured on
	// the ordinary path and not on the server-bug branch.
	assertNormalPath(t, s)
}

// TestReorderWatermarkAPollBetweenTheTwoSpendsStillDeliversTheLateArrival is the
// mechanism the SENDER used to choose its victim, run end to end and now closed.
//
// The reader does nothing unusual: it reads, is handed what exists, and advances
// its cursor exactly as the contract tells it to. The sender then spends the
// reservation it has been holding. Under the old design nothing the reader could
// have done differently would have saved it. Under the split the reader is
// served the message on its very next poll, from the cursor the store itself
// handed it.
func TestReorderWatermarkAPollBetweenTheTwoSpendsStillDeliversTheLateArrival(t *testing.T) {
	clock := newClock()
	s := newOutOfOrderStore(clock)
	alpha := agentIDFor(t, "alpha")
	beta := agentIDFor(t, "beta")

	// bob spends two reservations, at positions 1 and 2.
	mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 3, 1, clock.now(), "bob one"))
	mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 4, 2, clock.now(), "bob two"))

	// beta polls, is served both, and advances its cursor as instructed.
	batch, cursor, _ := s.Since(beta, noEpoch, 0, 16)
	if got := seqsOf(batch); !reflect.DeepEqual(got, []uint64{3, 4}) {
		t.Fatalf("the reader's first poll served %v, want [3 4]", got)
	}
	if cursor != 2 {
		t.Fatalf("the reader's first poll reported next = %d, want 2 — the POSITION of the last message in the "+
			"batch, which is where the contract tells it to resume", cursor)
	}

	// alice now spends the reservation she has been holding since before either
	// of bob's. It commits at the NEXT position, 3.
	late := mkMessageAt(t, alpha, true, nil, 1, 3, clock.now(), "alice, held and spent last")
	mustAppend(t, s, late)
	assertNormalPath(t, s)

	// Premise: the reader's cursor really is above alice's sequence, so this is
	// the shape that used to be unrecoverable and not a cursor that merely
	// happens to sit below it.
	if cursor <= late.Seq {
		t.Fatalf("PREMISE BROKEN: the reader's cursor is %d and alice's sequence is %d; this test needs a cursor "+
			"STRICTLY ABOVE the late arrival's sequence, which is the reader the old design lost", cursor, late.Seq)
	}

	// THE FIX. Same reader, the exact cursor the store handed it.
	served, next, more := s.Since(beta, noEpoch, cursor, 16)
	if got := seqsOf(served); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("the reader at the cursor it was given (%d) was served %v, want [1] — alice's message committed "+
			"LAST, so it carries position %d, above this cursor, and must be delivered "+
			"(SIGN-1-FU-REORDER-WATERMARK)", cursor, got, late.Pos)
	}
	if next != late.Pos {
		t.Fatalf("next = %d after serving the late arrival, want its position %d", next, late.Pos)
	}
	if more {
		t.Fatalf("more = true on a one-message batch with limit 16")
	}
	if !s.HasVisibleAfter(beta, noEpoch, cursor) {
		t.Fatalf("HasVisibleAfter(%d) is false while Since(%d) served the late arrival — a parked long poll would "+
			"decide to keep sleeping on a message its own read would return", cursor, cursor)
	}

	// ONCE, not for ever. Closing the gap must not turn into a reader that
	// re-reads the same message on every poll (invariant 10): from the cursor it
	// was just given there is nothing left, and POLL-1 leaves that cursor alone.
	if got, next2, _ := s.Since(beta, noEpoch, next, 16); len(got) != 0 || next2 != next {
		t.Fatalf("re-reading from the cursor the late delivery handed back (%d) served %v and reported next = %d; "+
			"want nothing and an unchanged cursor", next, seqsOf(got), next2)
	}

	// ORDINARY TRAFFIC CONTINUES NORMALLY on top of it.
	mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 5, 4, clock.now(), "later traffic"))
	got, next3, _ := s.Since(beta, noEpoch, next, 16)
	if seqs := seqsOf(got); !reflect.DeepEqual(seqs, []uint64{5}) {
		t.Fatalf("after later traffic the reader was served %v, want [5]", seqs)
	}
	if next3 != 4 {
		t.Fatalf("next = %d after serving seq 5 at position 4, want 4", next3)
	}

	// NOBODY IS SKIPPED, from the top, one message at a time, following only the
	// cursor the store hands back. This is the property the defect broke.
	var drained []uint64
	c := uint64(0)
	for round := 0; round < 8; round++ {
		b, n, _ := s.Since(beta, noEpoch, c, 1)
		if len(b) == 0 {
			break
		}
		drained = append(drained, b[0].Seq)
		if n <= c {
			t.Fatalf("a non-empty batch left the cursor at %d", c)
		}
		c = n
	}
	if want := []uint64{3, 4, 1, 5}; !reflect.DeepEqual(drained, want) {
		t.Fatalf("draining one at a time from cursor 0 yielded %v, want %v — every message must be reachable "+
			"from the cursor the store hands back, in delivery order", drained, want)
	}
	assertNormalPath(t, s)
}

// TestReorderWatermarkTheGapCanBeArbitrarilyWide is the REJECTION CRITERION for
// the design this task is named after, kept because it is the evidence, not the
// history.
//
// The watermark fix — hold every reader's cursor at (lowest outstanding mint − 1)
// — has a cost bounded by nothing in this package: while one reservation is
// unspent, the head can advance arbitrarily far above it. This test measures that
// distance directly, at 200 messages, and then shows what the split does instead:
// the reader at the head is served the straggler immediately, and was never
// stalled behind it for a moment.
//
// The hub-side half, which shows the same distance through hub.Mint and its
// fifteen-minute TTL, is
// TestReorderWatermarkHeadAdvancesFarWhileAMintIsOutstanding.
func TestReorderWatermarkTheGapCanBeArbitrarilyWide(t *testing.T) {
	const above = 200

	clock := newClock()
	s := newOutOfOrderStore(clock)
	alpha := agentIDFor(t, "alpha")
	beta := agentIDFor(t, "beta")

	// Sequence 1 is minted and NOT spent. Everything else commits above it, at
	// positions that are simply the arrival index.
	for i := 0; i < above; i++ {
		seq, pos := uint64(i+2), uint64(i+1)
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, seq, pos, clock.now(), fmt.Sprintf("m%d", seq)))
	}
	assertNormalPath(t, s)

	// A reader that keeps up is now at the head of the delivery order.
	batch, cursor, more := s.Since(beta, noEpoch, 0, above+16)
	if more {
		t.Fatalf("the catch-up read was cut short; raise the limit")
	}
	if len(batch) != above {
		t.Fatalf("the catch-up read served %d messages, want %d", len(batch), above)
	}
	if cursor != above {
		t.Fatalf("the catch-up reader's cursor is %d, want %d (the head POSITION — %d records have committed)",
			cursor, above, above)
	}

	// The reservation is finally spent, at the next position.
	late := mkMessageAt(t, alpha, true, nil, 1, above+1, clock.now(), "the message held back")
	mustAppend(t, s, late)
	assertNormalPath(t, s)

	// THE READER AT THE HEAD IS SERVED IT, one poll later, having been stalled by
	// nothing at all in the meantime.
	got, next, _ := s.Since(beta, noEpoch, cursor, 16)
	if seqs := seqsOf(got); !reflect.DeepEqual(seqs, []uint64{1}) {
		t.Fatalf("the head reader at cursor %d was served %v, want [1] — the straggler lands at position %d, above "+
			"every live cursor, and reaches everyone (SIGN-1-FU-REORDER-WATERMARK)", cursor, seqs, late.Pos)
	}
	if next != above+1 {
		t.Fatalf("next = %d after serving the straggler, want %d", next, above+1)
	}
	if _, _, head, dropped := statsHeadDropped(t, s); head != above+1 || dropped != 0 {
		t.Fatalf("Stats reports head = %d, dropped = %d, want head = %d and nothing pruned", head, dropped, above+1)
	}
	if got := s.PosHead(); got != above+1 {
		t.Fatalf("PosHead() = %d, want %d", got, above+1)
	}

	t.Logf("QUANTIFIED — WHY THE WATERMARK WAS REJECTED: with ONE unspent reservation at sequence 1 the head "+
		"reached sequence %d over %d positions. A fix that stalled readers at (lowest outstanding mint - 1) would "+
		"have withheld %d durable, acknowledged messages from EVERY reader on the bus until that one reservation "+
		"was spent or expired (hub.MintTTL, fifteen minutes). The seq/pos split withheld NOTHING: every one of the "+
		"%d was delivered as it committed, and the straggler was delivered on the next poll after it.",
		above+1, above, above, above)
}

// TestReorderWatermarkANonMonotonicPositionIsTheFaultBranchNotTheNormalPath
// pins the one path that still produces the OLD suppressing shape, and pins that
// it is loudly distinguishable from the ordinary late spend.
//
// A position that is NOT above every position already applied cannot be reached
// by any client — positions are WAL commit indices minted under the hub's write
// lock — so it means hub.publish has stopped holding writeMu across the durable
// write and the serving-copy append. Append must:
//
//   - RETAIN the message and RETURN NIL. The record is already committed and
//     fsynced (invariant 4); an error would orphan it and halt the bus, which is
//     the P0 SIGN-1-FU-OUTOFORDER-POISON fixed.
//   - be LOUD and COUNTED. Invariant 6 sanctions the discard, never the silence,
//     and a log line is not queryable.
//
// The residual asserted at the end — a reader already past that position is NOT
// served the message — is not a bug to fix here; it is exactly why the counter
// has to exist. It is also the shape the first version of this file measured
// while claiming to measure the normal path.
func TestReorderWatermarkANonMonotonicPositionIsTheFaultBranchNotTheNormalPath(t *testing.T) {
	// The CONTROL: the ordinary out-of-order spend, which shares the low sequence
	// and shares nothing else. It is silent, it is not counted, and it reaches the
	// reader that has already passed its sequence.
	t.Run("TheOrdinaryLateSpendIsNeitherCountedNorSuppressed", func(t *testing.T) {
		clock := newClock()
		var logBuf bytes.Buffer
		s := store.New(store.Options{Now: clock.now, Logger: logging.New(&logBuf, logging.LevelWarn)})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 3, 1, clock.now(), "bob"))
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 4, 2, clock.now(), "bob again"))
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 1, 3, clock.now(), "alice, late"))

		assertNormalPath(t, s)
		if got := logBuf.String(); got != "" {
			t.Fatalf("the ordinary late spend logged %q, want nothing — it is delivered to every reader, so there "+
				"is nothing to report, and a line here would train operators to ignore the one that matters", got)
		}
		if got := seqsOf(mustSince(t, s, beta, 2, 16)); !reflect.DeepEqual(got, []uint64{1}) {
			t.Fatalf("a reader at cursor 2 was served %v, want [1]", got)
		}
	})

	t.Run("TheFaultBranchRetainsServesAndCounts", func(t *testing.T) {
		clock := newClock()
		var logBuf bytes.Buffer
		s := store.New(store.Options{Now: clock.now, Logger: logging.New(&logBuf, logging.LevelWarn)})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 3, 10, clock.now(), "committed at position 10"))
		logBuf.Reset()

		// Position 4 is BELOW the position already applied. No client can steer a
		// message here; only a write path that has stopped serialising can.
		bad := mkMessageAt(t, alpha, true, nil, 1, 4, clock.now(), "applied out of order")
		if err := s.Append(bad); err != nil {
			t.Fatalf("Append with a non-monotone position returned %v; it must return nil — the record is already "+
				"committed and fsynced, so an error orphans it on disk and poisons the hub", err)
		}

		// RETAINED and SERVED, in position order so the binary search stays honest.
		if got := seqsOf(mustSince(t, s, beta, 0, 16)); !reflect.DeepEqual(got, []uint64{1, 3}) {
			t.Fatalf("after a non-monotone append the stream is %v, want [1 3] — positions 4 then 10. Dropping the "+
				"message would be the silent suppression this task removes (invariant 6)", got)
		}
		if got := s.NonMonotonicPositions(); got != 1 {
			t.Fatalf("NonMonotonicPositions() = %d, want 1 — the condition must be OBSERVABLE, not log-only. This "+
				"counter is also the guard that keeps this file honest: every normal-path test asserts it is 0", got)
		}
		if got := s.PosHead(); got != 10 {
			t.Fatalf("PosHead() = %d, want 10 — the position high-water mark never rewinds", got)
		}

		// LOUD. Asserted on the logfmt pairs, so rewording the prose does not break
		// the test while DELETING the call does.
		logLine := logBuf.String()
		for _, want := range []string{"level=error", "pos=4", "pos_head=10", "non_monotonic_total=1"} {
			if !strings.Contains(logLine, want) {
				t.Fatalf("the non-monotone-position log does not contain %q; this line is the ONLY narrative signal "+
					"that the write path's locking has stopped holding (invariant 6). Log was:\n%s", want, logLine)
			}
		}

		// THE RESIDUAL, and the reason the counter is the alarm rather than the
		// log: a reader already past position 4 never sees this message. That is
		// the OLD suppressing shape, reachable now only through a server bug — and
		// it is what a fixture stamping Pos == Seq accidentally measures.
		if got := seqsOf(mustSince(t, s, beta, 10, 16)); len(got) != 0 {
			t.Fatalf("a reader at cursor 10 was served %v; want nothing. If this changed, Append has started "+
				"repairing non-monotone positions, and this test — plus the counter it pins — needs revising "+
				"rather than deleting", got)
		}
	})
}

// TestReorderWatermarkThePrunedRegionReserveGuardIsGoneAndThatIsTheTRADE pins
// the one property this task DELETED, so the narrowing is a recorded decision
// with a test behind it rather than a silent regression.
//
// # What was removed
//
// Append used to refuse to RETAIN any message whose SEQUENCE was at or below
// prunedHead — the highest sequence retention had already dropped. Its documented
// job was P1's second half: across the region retention has already pruned, a
// re-arriving sequence was PREVENTED from being served a second time, without
// being DETECTED. It could not be detected, because a high-water mark cannot tell
// a double-apply from a very late first arrival — both are just "a number below
// the mark". Prevention was all it could offer.
//
// # Why it had to go, and what was traded for what
//
// The two cases the mark cannot tell apart are the two subtests below, and they
// are byte-identical to the store:
//
//  1. A sequence already SERVED and then pruned, arriving AGAIN. The refusal
//     stopped this, and now nothing does: it is retained and served a second time.
//  2. A sequence minted before the pruned window and spent NOW — a genuinely late
//     FIRST arrival, never served to anyone. This is the ordinary shape of the
//     protocol after SIGN-1 (a reservation may be held for hub.MintTTL) and it is
//     the message this whole task exists to deliver. The refusal dropped it, which
//     is the prune-race the security gate found.
//
// So the refusal could not keep (1) without also dropping (2), and (2) is an
// ACKNOWLEDGED, durable message being silently discarded — invariant 4's promise
// broken in the delivery plane, which is worse than the harm in (1). Under
// delivery ordering (1) is also strictly less harmful than it was: the re-arrival
// goes to the TAIL, above every live cursor, so it is a duplicate DELIVERY of a
// message nobody can still be holding a cursor into, not a corruption of the
// stream. The trade is deliberate; DECISIONS.md carries the prose half.
//
// # What is NOT narrowed
//
// Detection INSIDE the retained window is untouched — bySeq still reports a genuine
// double-apply as ErrDuplicateSequence, which is the subtest that closes this
// test. Only the pruned region, where no evidence survives by construction, lost
// its guard.
func TestReorderWatermarkThePrunedRegionReserveGuardIsGoneAndThatIsTheTRADE(t *testing.T) {
	// tenBytes is exactly ten bytes, so a MaxBytes of 20 holds precisely two
	// messages and the third append prunes the first. Same machinery as
	// TestStoreRetention/ByBytes.
	const tenBytes = "0123456789"

	// prunedFixture is the shared setup: four messages commit in arrival order at
	// positions 1..4, a reader is served all four across two polls, and bytes
	// retention drops sequences 10 and 11 on the way. It returns the store, the
	// reader, and the reader's cursor — which is a POSITION.
	//
	// The reader has therefore ALREADY BEEN SERVED sequences 10 and 11 and they
	// are no longer in bySeq: the store has forgotten them completely.
	prunedFixture := func(t *testing.T) (*store.Store, string, uint64) {
		t.Helper()
		clock := newClock()
		// Age is slack so BYTES alone decide what is dropped.
		s := store.New(store.Options{MaxAge: 24 * time.Hour, MaxBytes: 20, Now: clock.now})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 10, 1, clock.now(), tenBytes))
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 11, 2, clock.now(), tenBytes))

		// SERVED. This is what makes case (1) below a genuine RE-serve rather than
		// a first delivery.
		batch, cursor, _ := s.Since(beta, noEpoch, 0, 16)
		if got := seqsOf(batch); !reflect.DeepEqual(got, []uint64{10, 11}) {
			t.Fatalf("PREMISE BROKEN: the reader's first poll served %v, want [10 11]", got)
		}

		// Ordinary traffic pushes both out of the retention window.
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 12, 3, clock.now(), tenBytes))
		mustAppend(t, s, mkMessageAt(t, alpha, true, nil, 13, 4, clock.now(), tenBytes))
		assertNormalPath(t, s)

		count, _, oldest, head, dropped := s.Stats()
		if count != 2 || oldest != 12 || head != 13 || dropped != 2 {
			t.Fatalf("PREMISE BROKEN: Stats = (count %d, oldest %d, head %d, dropped %d), want (2, 12, 13, 2) — "+
				"sequences 10 and 11 must have been PRUNED, or there is no pruned region to test",
				count, oldest, head, dropped)
		}

		batch, cursor, _ = s.Since(beta, noEpoch, cursor, 16)
		if got := seqsOf(batch); !reflect.DeepEqual(got, []uint64{12, 13}) {
			t.Fatalf("PREMISE BROKEN: the reader's second poll served %v, want [12 13]", got)
		}
		if cursor != 4 {
			t.Fatalf("PREMISE BROKEN: the caught-up reader's cursor is %d, want the head POSITION 4", cursor)
		}
		return s, beta, cursor
	}

	// CASE 1 — THE NARROWING ITSELF. What the deleted branch used to prevent now
	// happens, and this test says so out loud.
	t.Run("AnAlreadyServedThenPrunedSequenceIsRetainedAndSERVEDASECONDTIME", func(t *testing.T) {
		s, beta, cursor := prunedFixture(t)

		// Sequence 11 AGAIN. It was served to this very reader in the fixture and
		// then pruned, so bySeq no longer holds it and nothing in the store
		// remembers it existed. Its POSITION is fresh and monotone (5), which is
		// what keeps this on the ORDINARY path — only the sequence is old.
		again := mkMessageAt(t, agentIDFor(t, "alpha"), true, nil, 11, 5, newClock().now(), tenBytes)
		if err := s.Append(again); err != nil {
			t.Fatalf("appending sequence 11 again after it was pruned returned %v, want nil. The at-or-below-"+
				"prunedHead refusal was REMOVED on SIGN-1-FU-REORDER-WATERMARK: it could not tell this apart from "+
				"a genuinely late first arrival, and refusing both discarded acknowledged mail (invariant 4). If "+
				"this is failing, the refusal has come back and the prune-race is open again", err)
		}

		// NORMAL PATH, not the server-bug branch: the position is above every
		// position applied, so nothing here is measuring the non-monotone fault.
		assertNormalPath(t, s)

		// RETAINED, and SERVED — a second time — to the reader at the cursor the
		// store itself handed it one poll ago.
		served, next, more := s.Since(beta, noEpoch, cursor, 16)
		if got := seqsOf(served); !reflect.DeepEqual(got, []uint64{11}) {
			t.Fatalf("the reader at cursor %d was served %v, want [11]. THIS IS THE ACCEPTED NARROWING: sequence 11 "+
				"was already delivered to this reader and then pruned, and the store — which keeps no evidence past "+
				"the retained window — serves it again. Prevention in the pruned region was traded for delivery of "+
				"the genuinely late arrival (SIGN-1-FU-REORDER-WATERMARK)", cursor, got)
		}
		if next != 5 {
			t.Fatalf("next = %d after serving the re-arrival, want its position 5", next)
		}
		if more {
			t.Fatalf("more = true on a one-message batch with limit 16")
		}

		// AND THE SEQUENCE HEAD DOES NOT REWIND (invariant 1). A late lower number
		// must never drag the high-water mark back, or the next number the bus
		// hands out is one it has already used.
		if _, _, head, _ := statsHeadDropped(t, s); head != 13 {
			t.Fatalf("Stats reports head = %d after a sequence BELOW the head re-arrived, want 13 — the sequence "+
				"high-water mark only ever grows (invariant 1)", head)
		}
		if got := s.PosHead(); got != 5 {
			t.Fatalf("PosHead() = %d, want 5 — the re-arrival took the next DELIVERY POSITION like any other "+
				"message; that is what puts it above every live cursor", got)
		}
	})

	// CASE 2 — WHAT THE TRADE BOUGHT. Identical to case 1 from the store's point
	// of view, and the whole reason the guard could not be kept.
	t.Run("AGenuinelyLateFirstArrivalBelowThePrunedSequenceIsDelivered", func(t *testing.T) {
		s, beta, cursor := prunedFixture(t)

		// Sequence 5 was minted before any of the fixture's messages and has NEVER
		// been spent, so it has never been served to anybody. It is below every
		// sequence retention dropped, which is exactly where the deleted branch
		// refused — and refusing here would have discarded an acknowledged,
		// fsynced message that no reader had ever seen.
		late := mkMessageAt(t, agentIDFor(t, "alpha"), true, nil, 5, 5, newClock().now(), tenBytes)
		if err := s.Append(late); err != nil {
			t.Fatalf("appending the never-spent sequence 5 after the pruned window returned %v, want nil — this is "+
				"the message the whole task exists to deliver, and the store CANNOT tell it apart from the "+
				"re-arrival in the case above. That indistinguishability is precisely why the refusal had to go", err)
		}
		assertNormalPath(t, s)

		served, next, _ := s.Since(beta, noEpoch, cursor, 16)
		if got := seqsOf(served); !reflect.DeepEqual(got, []uint64{5}) {
			t.Fatalf("the caught-up reader at cursor %d was served %v, want [5] — the late arrival lands at "+
				"position 5, above every live cursor, and reaches every reader", cursor, got)
		}
		if next != 5 {
			t.Fatalf("next = %d after serving the late arrival, want its position 5", next)
		}
	})

	// CASE 3 — WHAT IS NOT NARROWED. Inside the retained window a genuine
	// double-apply is still DETECTED and still loud, which is the half of P1 that
	// survived intact.
	t.Run("DetectionInsideTheRetainedWindowIsUnaffected", func(t *testing.T) {
		s, _, _ := prunedFixture(t)

		// Sequence 13 is still retained, so bySeq still holds it.
		dup := mkMessageAt(t, agentIDFor(t, "alpha"), true, nil, 13, 6, newClock().now(), tenBytes)
		if err := s.Append(dup); !errors.Is(err, store.ErrDuplicateSequence) {
			t.Fatalf("re-appending the RETAINED sequence 13 returned %v, want ErrDuplicateSequence. Removing the "+
				"pruned-region refusal narrowed P1 across the region retention has already dropped and NOWHERE "+
				"else; within the retained window a double-apply is still detected out of bySeq, and the hub still "+
				"poisons itself on it (SIGN-1-FU-REORDER-WATERMARK)", err)
		}
		if got := s.PosHead(); got != 4 {
			t.Fatalf("PosHead() = %d after a REFUSED append, want 4 — a rejected message must not move any "+
				"high-water mark", got)
		}
	})
}

// statsHeadDropped is Stats with the two fields these tests care about named, so
// a failure message does not have to decode a five-value tuple.
func statsHeadDropped(t *testing.T, s *store.Store) (count int, oldest uint64, head uint64, dropped uint64) {
	t.Helper()
	count, _, oldest, head, dropped = s.Stats()
	return count, oldest, head, dropped
}

// nonEmpty normalises a nil slice to an empty one so reflect.DeepEqual can
// compare "served nothing" against a nil expectation without the test author
// having to remember which shape seqsOf produces.
func nonEmpty(s []uint64) []uint64 {
	if s == nil {
		return []uint64{}
	}
	return s
}
