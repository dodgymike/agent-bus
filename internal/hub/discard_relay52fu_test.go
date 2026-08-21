package hub_test

// INVARIANT 6, THE LOUD HALF, ON THE HUB DISCARD LINES RELAY-52 DID NOT REACH
// (RELAY-52-FU-HUBDISCARDS).
//
// RELAY-52 tested ONE of Hub.Apply's discard branches: the message record that
// does not DECODE. Four others existed and none of them was asserted to fire:
//
//	hub.go, Apply               a message record that decodes but cannot be
//	                            APPLIED to the serving copy
//	hub.go, Apply               an applied-key record that decodes but cannot be
//	                            REMEMBERED
//	hub.go, recoverIdemRecord   an applied-key record that does not DECODE
//	hub.go, recoverIdemRecord   an applied-key record REBUILT from a pre-IDEM-11
//	                            message record whose result will not encode
//
// The first three are covered here. The FOURTH IS UNREACHABLE BY CONSTRUCTION
// and is deliberately not faked — see hubfuUnreachableRebuildNote below, which
// is the honest record of that rather than a green test over nothing.
//
// # "PRESENT BUT HOLLOW" IS THE FAILURE MODE THIS FILE EXISTS FOR
//
// The apply-discard line was ALREADY named by a test before this file:
// outoforder_poison_sign1fu_test.go's discardOnApplyLine. But its only use
// asserts the line is ABSENT — that a correctly-behaving bus never emits it.
// That is WEAKER THAN UNTESTED, because grep says "covered": deleting the log
// call outright, or hollowing it out until it named no record, made that
// assertion MORE likely to pass. Every subtest below asserts a discard line
// FIRES, and asserts the fields an operator would need to act on it.
//
// # THE ONE-WORD COLLAPSE
//
// Two of these lines differ from each other by a single word:
//
//	"a message record that could not be DECODED during recovery; …"
//	"a message record that could not be APPLIED during recovery; …"
//
// and three more differ only in a middle phrase ("could not be decoded" /
// "could not be remembered" / "rebuilt from a pre-IDEM-11 message record").
// Nothing mechanically stops a future edit collapsing one onto another's
// wording, after which a test matching by substring passes on the wrong branch.
// So every constant here is matched on the FULL, EXACT msg field, and
// TestMessageRecordDiscardLinesAreNotInterchangeable drives each branch in turn
// and asserts the OTHER line is absent — the 2x2 that makes a collapse red in
// both directions.
//
// # INVARIANTS READ IN FULL BEFORE WRITING THIS
//
// 6 (recovery ALWAYS reaches a running server; damaged records are discarded
// and the discard is logged loudly and SPECIFICALLY — silent discard is the
// defect), 4 and its 2026-08-02 narrowing (durability before ack covers OUR
// write path, not damaged media), 5 (memory is the serving copy, disk is the
// truth) and 1 (ids and sequences are never reused and never rewind, and a
// discard advances past the hole).

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// The lines under test, quoted IN FULL and matched on the whole msg field.
//
// They are separate constants from discard_relay52_test.go's, and the decode
// line is deliberately restated here rather than borrowed: this file's whole
// subject is that these strings are distinguishable, and a file that asserts a
// distinction has to own both sides of it.
const (
	// Apply, when store.Append refuses a record that decoded cleanly.
	hubfuApplyDiscardLine = "DISCARDING a message record that could not be applied during recovery; it is not in this bus's history and will not be delivered"
	// Apply, when store.Decode refuses the record. RELAY-52's subject; restated
	// here as the line that must NOT appear when the fault is an apply fault.
	hubfuDecodeDiscardLine = "DISCARDING a message record that could not be decoded during recovery; it is not in this bus's history and will not be delivered"
	// Apply, when the message applied but its applied-key record will not go
	// into the table.
	hubfuIdemRememberDiscardLine = "DISCARDING an applied-key record that could not be remembered during recovery; a retry of this key will be applied as a NEW operation"
	// recoverIdemRecord, when the entry's idem payload will not decode.
	hubfuIdemDecodeDiscardLine = "DISCARDING an applied-key record that could not be decoded during recovery; a retry of this key will be applied as a NEW operation"
	// recoverIdemRecord, the pre-IDEM-11 rebuild path. See
	// hubfuUnreachableRebuildNote.
	hubfuIdemRebuildDiscardLine = "DISCARDING an applied-key record rebuilt from a pre-IDEM-11 message record; a retry of this key will be applied as a NEW operation"
	// Apply's ONE-SHOT cap notice for the DECODE line. Named here so the apply
	// branch can assert it does not borrow the decode branch's cap.
	hubfuDecodeCapLine = "further undecodable message records will NOT be logged individually; the total is reported once recovery finishes"
	// The end-of-recovery summary (roster.go, noteRecoveredIdentities), matched
	// by PREFIX. Named here so the APPLY branch can assert it is not polluted by
	// a fault that has nothing to do with decoding.
	hubfuSummaryPrefix = "THE AGENT-ID REUSE CHECK RAN ON INCOMPLETE INPUT"
)

// hubfuUnreachableRebuildNote records why hubfuIdemRebuildDiscardLine has no
// positive test, so that the gap is a documented finding rather than an
// oversight somebody re-discovers.
//
// That line fires only when encodeStoredResult returns an error, and
// encodeStoredResult is json.Marshal over a struct of string, uint64, []string
// and string. encoding/json cannot fail on those: there is no channel, no
// func, no cyclic pointer and no Marshaler to return an error, and invalid
// UTF-8 in a string is REPLACED rather than refused. The branch is therefore
// defence in depth over an impossibility, and NO fixture reachable through the
// exported surface can drive it. Writing a test that "covers" it would mean
// asserting something else entirely and labelling it with this line's name,
// which is precisely the hollow-coverage failure this file exists to remove.
//
// What IS asserted, in TestPreIdem11AppliedKeyThatCannotBeRememberedIsDiscardedLoudly,
// is that the surrounding pre-IDEM-11 REBUILD PATH really executes — the record
// is rebuilt from the message's own fields, encodeStoredResult is called and
// succeeds, and the rebuilt record then reaches idem.Recover. So the path is
// live; only its impossible error arm is untested.
const hubfuUnreachableRebuildNote = hubfuIdemRebuildDiscardLine

// hubfuBaseSeq is where the hand-built records start. It sits far above
// anything the hub itself mints in these fixtures, and the fixture GUARDS that
// rather than assuming it: a hand-built record that collided with a genuinely
// minted sequence would produce the duplicate-sequence discard for the wrong
// reason and this file would be testing its own fixture.
const hubfuBaseSeq uint64 = 1000

// hubfuGoodBody identifies the one message that really was accepted, which
// every subtest must still be able to read back afterwards.
const hubfuGoodBody = "the accepted message that must survive every discard below"

// hubfuUnacceptableKey is an idempotency key that store.Decode ACCEPTS and
// idem.ValidateKey REFUSES.
//
// That asymmetry is real and is the whole trigger for the "could not be
// remembered" branch: store bounds the key's LENGTH only (a durable record is
// not re-adjudicated against a charset rule that may have changed), while idem
// enforces KeyCharset. A record written by a build with a laxer rule — or
// handed over by a peer, or read off damaged media — therefore decodes into a
// perfectly good message whose applied-key record cannot be remembered.
// hubfuAssertRememberPremise proves both halves before the fixture is trusted.
const hubfuUnacceptableKey = "k relay52fu/remember"

// hubfuRecovered is a bus recovered over a log holding one genuinely accepted
// message followed by hand-built records.
type hubfuRecovered struct {
	h *hub.Hub
	// log is everything the hub logged from Open onwards.
	log string
	// injected is what the WAL reported for each hand-built entry, in the order
	// they were appended, so assertions compare against the REAL indices rather
	// than against a constant the production line could also be printing.
	injected []wal.Committed
	// good identifies the message that must still be served afterwards.
	good hub.Result
	// appliedSeq is the HIGHEST sequence the fixture proves a replayed record
	// was written at — 0 when the fixture's hand-built records were all
	// discarded and only the genuinely accepted message survived.
	//
	// It exists so hubfuAssertRunning can hold the mint to the strongest floor
	// the fixture actually establishes rather than to the weakest one. See the
	// invariant 1 note there.
	appliedSeq uint64
	// alpha sends, beta receives.
	alpha, beta string
}

// hubfuRecover builds the fixture and reopens the bus over it.
//
// The shape mirrors discard_relay52_test.go's relay52Recover deliberately —
// real accepted history first, damage appended directly to the durable log
// second, restart third — but it takes ARBITRARY entries rather than a count of
// undecodable ones, because every branch in this file needs a differently
// shaped entry (a valid body with a duplicate sequence, a valid body with a
// broken idem payload, a valid body with a key idem refuses). It is a separate
// function rather than an edit to that file so the two tasks' fixtures cannot
// break each other.
//
// build returns the entries to inject AND the highest sequence among them it
// expects to be successfully APPLIED (0 when it expects none). That second
// value is not bookkeeping: it is what lets hubfuAssertRunning hold the
// recovered mint to the real floor the fixture established, rather than to the
// single low sequence the accepted message happens to carry.
func hubfuRecover(t *testing.T, build func(t *testing.T, alpha, beta string) ([]wal.Entry, uint64)) hubfuRecovered {
	t.Helper()
	dir := t.TempDir()
	alpha := agentID(t, testBusID, "alpha")
	beta := agentID(t, testBusID, "beta")

	// (1) A REAL accepted history: minted, sent, committed and fsynced by the
	// hub itself, so recovery has something legitimate to rebuild and every
	// "the bus is still running" assertion is about a bus that really did
	// recover state.
	lg := openTestLog(t, dir, false)
	h, _ := openMintHub(t, dir, lg, nil, "", "alpha", "beta")
	good, err := mintedSend(t, h, hub.SendRequest{
		Sender:         alpha,
		To:             beta,
		Body:           []byte(hubfuGoodBody),
		IdempotencyKey: "k-relay52fu-good",
	})
	if err != nil {
		t.Fatalf("building the accepted history: Send: %v", err)
	}
	if good.Seq >= hubfuBaseSeq {
		t.Fatalf("PREMISE BROKEN: the hub minted sequence %d for the accepted message, at or above the %d this fixture's hand-built records start at. The hand-built records would then collide with genuinely minted history and any duplicate-sequence discard below would be an artefact of the fixture rather than of the damage it injects.", good.Seq, hubfuBaseSeq)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("building the accepted history: closing the log: %v", err)
	}

	// (2) The damage, appended DIRECTLY to the durable log. The hub would never
	// write these itself, which is the point: they model records written by
	// another build, handed over by a peer, or left by damaged media.
	entries, appliedSeq := build(t, alpha, beta)
	if len(entries) == 0 {
		t.Fatalf("the fixture injected no entries at all; it would assert nothing about any discard branch")
	}
	lg2 := openTestLog(t, dir, false)
	injected := make([]wal.Committed, 0, len(entries))
	for i, e := range entries {
		c, err := lg2.Write(e)
		if err != nil {
			t.Fatalf("appending hand-built record %d: %v", i, err)
		}
		injected = append(injected, c)
	}
	if err := lg2.Close(); err != nil {
		t.Fatalf("closing the log after appending the hand-built records: %v", err)
	}

	// (3) THE RESTART. Replay drives Hub.Apply over the whole log.
	lg3 := openTestLog(t, dir, true)
	h2, buf, err := tryOpenMintHub(t, dir, lg3, nil, "", "alpha", "beta")
	if err != nil {
		t.Fatalf("hub.Open over a log holding %d hand-built record(s) REFUSED TO START: %v\n\nInvariant 6 settled this trade: recovery ALWAYS reaches a running server and the damaged records are discarded loudly. Failing closed here turns one bad record into an outage, and an operator has no way to get the bus back.\nlog: %s", len(entries), err, buf.String())
	}
	return hubfuRecovered{h: h2, log: buf.String(), injected: injected, good: good, appliedSeq: appliedSeq, alpha: alpha, beta: beta}
}

// ---------------------------------------------------------------------------
// hub.go, Apply: a record that DECODES but cannot be APPLIED
// ---------------------------------------------------------------------------

// TestMessageRecordThatCannotBeAppliedIsDiscardedLoudlyAndTheBusStarts drives
// the branch outoforder_poison_sign1fu_test.go only ever asserted the ABSENCE
// of.
//
// # THE TRIGGER IS THE ONE store.Append ACTUALLY REFUSES
//
// store.Append refuses exactly three things: sequence 0 and position 0, neither
// of which replay can produce (store.Decode rejects sequence 0, and the
// position is the WAL commit index, which is never 0), and a DUPLICATE
// SEQUENCE, which it calls a P1 because it means the server handed the same id
// out twice. A non-monotone POSITION is explicitly NOT an error there any more
// (SIGN-1-FU-REORDER-WATERMARK): it is retained and logged by the store. So a
// duplicated message record is the reachable trigger, and it is a real one — a
// log whose tail was re-appended, or a build that violated invariant 1.
//
// # THE FLOOD IS SIZED FROM THE DECODE CAP, NOT FROM A LITERAL
//
// The decode branch next door is CAPPED, and this one is not. "Not capped" is
// only demonstrable by exceeding whatever cap exists, so the fixture measures
// the decode cap from a log of its own and then injects more than that many
// un-appliable records. Hard-coding 12 here would silently stop proving
// anything the day the cap moved.
func TestMessageRecordThatCannotBeAppliedIsDiscardedLoudlyAndTheBusStarts(t *testing.T) {
	decodeCap := hubfuMeasureDecodeCap(t)
	unappliable := decodeCap + 3

	var dupID string
	f := hubfuRecover(t, func(t *testing.T, alpha, beta string) ([]wal.Entry, uint64) {
		t.Helper()
		body, id := hubfuMessageRecord(t, hubfuBaseSeq, alpha, beta, "the record that is present twice", "k-relay52fu-dup")
		dupID = id
		// The FIRST copy applies cleanly; every copy after it is the duplicate.
		out := make([]wal.Entry, 0, unappliable+1)
		for i := 0; i < unappliable+1; i++ {
			out = append(out, wal.Entry{Kind: store.RecordKind, Body: body})
		}
		return out, hubfuBaseSeq
	})
	hubfuAssertRunning(t, f)

	lines := relay52Select(t, f.log, hubfuApplyDiscardLine)
	if len(lines) != unappliable {
		t.Fatalf("%d copies of one message record were appended, so %d of them cannot be applied, but %d %q line(s) were emitted.\nInvariant 6 permits the discard and forbids it being SILENT: every record thrown away has to be named, and this branch carries no cap and no one-shot summary to fall back on, so a line that is not emitted here is a record nobody will ever learn was dropped.\nlog: %s", unappliable+1, unappliable, len(lines), hubfuApplyDiscardLine, f.log)
	}
	// EVERY line names ITS OWN record. The injected slice is offset by one
	// because injected[0] is the copy that applied.
	for i, fields := range lines {
		hubfuAssertNamesAppliedRecord(t, fields, f.injected[i+1], dupID, hubfuBaseSeq, f.log)
	}
	// THIS BRANCH DOES NOT BORROW THE DECODE BRANCH'S CAP. If it ever did, an
	// operator would be told nothing about the records past it, because the
	// cap's one-shot notice and the end-of-recovery total both count UNDECODABLE
	// records and neither counts these.
	if len(lines) <= decodeCap {
		t.Fatalf("%d records could not be applied but only %d were named individually, and the undecodable-record cap measured on this build is %d. This branch is uncapped by design; if a cap is being added it needs its own one-shot notice and its own total, exactly as the decode branch has, or the records past the cap are discarded silently.\nlog: %s", unappliable, len(lines), decodeCap, f.log)
	}
	if caps := relay52Select(t, f.log, hubfuDecodeCapLine); len(caps) != 0 {
		t.Fatalf("records that could not be APPLIED emitted the UNDECODABLE-record cap notice %d time(s); the two counters are different facts and sharing one would tell an operator that undecodable records were suppressed when none existed.\nlog: %s", len(caps), f.log)
	}
	// AND THEY ARE NOT COUNTED AS UNDECODABLE EITHER. Every record in this
	// fixture DECODED perfectly; they were refused by the serving copy. Counting
	// them in undecodable_message_records would make the end-of-recovery summary
	// report a record-schema problem that did not happen, and would fire the
	// id-reuse "INCOMPLETE INPUT" warning on a bus whose records all decoded.
	//
	// The assertion is on the COUNT rather than on the summary's absence, so that
	// the real gap here stays open rather than being frozen shut: these ids WERE
	// missed by the id-reuse harvest, and a future fix should report that through
	// a field of its OWN. Inflating this one is the thing that must not happen.
	hubfuAssertUndecodableTotal(t, f, 0)
}

// hubfuAssertUndecodableTotal pins what the end-of-recovery summary claims about
// UNDECODABLE records. want==0 means the summary must either be absent or report
// zero: it is a statement about records store.Decode refused, and nothing else
// may be folded into it.
func hubfuAssertUndecodableTotal(t *testing.T, f hubfuRecovered, want int) {
	t.Helper()
	for _, fields := range relay52Records(t, f.log) {
		if !strings.HasPrefix(fields["msg"], hubfuSummaryPrefix) {
			continue
		}
		got := relay52Field(t, fields, "undecodable_message_records", f.log)
		if got != strconv.Itoa(want) {
			t.Fatalf("the end-of-recovery summary reports undecodable_message_records=%s, want %d. Every record this fixture injected DECODED; they were refused by the SERVING COPY. Counting them here tells an operator to expect a record-schema problem, and raises the id-reuse INCOMPLETE INPUT warning, on a bus where every record decoded.\nlog: %s", got, want, f.log)
		}
		return
	}
	if want != 0 {
		t.Fatalf("recovery discarded %d undecodable record(s) and emitted no %q summary at all.\nlog: %s", want, hubfuSummaryPrefix, f.log)
	}
}

// hubfuAssertNamesAppliedRecord is the SPECIFICITY half of "loud and specific"
// for the apply branch: the line must let an operator find the record that was
// thrown away, and say what it was.
//
// It checks FIELDS against the values the WAL and the record itself actually
// carry. A substring check on the message text passes just as happily against a
// line naming no record at all, which is the shape that makes a discard
// effectively silent even though something was printed.
//
// pos is checked against the COMMIT index specifically. Apply stamps
// m.Pos = c.CommitIndex before appending, and the position is what a client's
// cursor points at, so a line reporting the prepare index there — or a constant
// — sends an operator looking in the wrong place in the log.
func hubfuAssertNamesAppliedRecord(t *testing.T, fields map[string]string, c wal.Committed, wantID string, wantSeq uint64, log string) {
	t.Helper()
	if lvl := fields["level"]; lvl != "error" {
		t.Fatalf("the discard was logged at level=%q, want error; invariant 6 requires it to be LOUD, and a discard reported below error is one an operator's filters drop.\nlog: %s", lvl, log)
	}
	for _, want := range []struct {
		key string
		val string
	}{
		{"prepare_index", strconv.FormatUint(c.PrepareIndex, 10)},
		{"message_id", wantID},
		{"seq", strconv.FormatUint(wantSeq, 10)},
		{"pos", strconv.FormatUint(c.CommitIndex, 10)},
	} {
		if got := relay52Field(t, fields, want.key, log); got != want.val {
			t.Fatalf("the discard line reports %s=%s, want %s. The line must name the record that was thrown away and where it sits: without its WAL index, its id, its sequence and its delivery position, \"a record was discarded\" is not an actionable report.\nlog: %s", want.key, got, want.val, log)
		}
	}
	// The REASON, in the operator's hands, and it must carry the DISTINGUISHING
	// SUBSTANCE rather than merely be non-empty.
	//
	// Non-empty alone was the assertion here first, and the reviewer killed it:
	// replacing `"err", err` with a fixed string SURVIVED, so the one field that
	// separates "this log has a duplicated tail" from "this build handed the same
	// sequence out twice" could be hollowed out undetected. The store names the
	// colliding SEQUENCE in its refusal, so that is what is required — a constant
	// cannot contain the sequence of a record it does not know about.
	// BOTH substances are required, and the second is why.
	//
	// The SEQUENCE alone is not enough: ids.MessageID is busID + "-" + seq, so
	// the message id already contains the same digits, and `"err", "cannot
	// append message "+m.ID` SURVIVED as a constant-shaped reason (reviewer N7).
	// store's refusal also says WHAT went wrong, and that phrase is the part a
	// reason invented at the call site cannot fake.
	hubfuAssertErrExplains(t, fields, log, strconv.FormatUint(wantSeq, 10), "is already applied")
}

// hubfuAssertErrExplains requires the err field to be present, non-empty, and to
// contain the substance that makes it actionable — the number, key or field name
// that identifies WHICH fault this was.
//
// It exists because "err is non-empty" is a hollow assertion: it passes against
// any fixed string, and a discard whose reason is a constant tells an operator
// nothing an absent field would not.
func hubfuAssertErrExplains(t *testing.T, fields map[string]string, log string, want ...string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("hubfuAssertErrExplains was given nothing to require; an err assertion with no required substance is the hollow assertion it exists to replace")
	}
	e := relay52Field(t, fields, "err", log)
	if strings.TrimSpace(e) == "" {
		t.Fatalf("the discard line carries an empty err field; without the reason, a discard is a report that something was thrown away and nothing an operator can act on.\nlog: %s", log)
	}
	for _, w := range want {
		if !strings.Contains(e, w) {
			t.Fatalf("the discard line reports err=%q, which does not mention %q. The err field must carry the SUBSTANCE that identifies this fault — a reason that is constant across every discard, or that only repeats what another field already says, is no more use than an absent one.\nlog: %s", e, w, log)
		}
	}
}

// ---------------------------------------------------------------------------
// hub.go, recoverIdemRecord: an applied-key record that does not DECODE
// ---------------------------------------------------------------------------

// TestAppliedKeyRecordThatCannotBeDecodedIsDiscardedLoudlyAndTheMessageSurvives
// drives recoverIdemRecord's decode-error branch.
//
// # THE BLAST RADIUS IS THE POINT
//
// Invariant 6 sanctions a discard; it does not sanction a WIDER one than the
// fault. The message on this entry is already applied by the time the idem
// payload is looked at, so the correct outcome is: the message is served, the
// KEY alone is lost, and the operator is told exactly which message's key it
// was. A future "simplification" that discarded the whole entry would lose an
// acknowledged message to protect one applied key, and every assertion about
// the log line would still pass — so the surviving message is asserted here,
// not just the line.
func TestAppliedKeyRecordThatCannotBeDecodedIsDiscardedLoudlyAndTheMessageSurvives(t *testing.T) {
	const body = "the message whose applied-key record is unreadable"
	var msgID string
	f := hubfuRecover(t, func(t *testing.T, alpha, beta string) ([]wal.Entry, uint64) {
		t.Helper()
		rec, id := hubfuMessageRecord(t, hubfuBaseSeq, alpha, beta, body, "k-relay52fu-idemdecode")
		msgID = id
		return []wal.Entry{{
			Kind: store.RecordKind,
			Body: rec,
			Idem: hubfuFutureIdemRecord(t, alpha, "k-relay52fu-idemdecode"),
		}}, hubfuBaseSeq
	})
	hubfuAssertRunning(t, f)

	lines := relay52Select(t, f.log, hubfuIdemDecodeDiscardLine)
	if len(lines) != 1 {
		t.Fatalf("one entry carried an applied-key record this build cannot decode, and %d %q line(s) were emitted, want exactly 1. Invariant 6 permits the discard and forbids it being silent — and this one is never summarised anywhere else, so a missing line is a lost idempotency guarantee nobody is told about.\nlog: %s", len(lines), hubfuIdemDecodeDiscardLine, f.log)
	}
	// "ratchet_epoch" is the unknown field the fixture added: the decoder names it,
	// which is what tells an operator a NEWER BUILD wrote this record rather than
	// that the bytes are damaged.
	hubfuAssertNamesIdemRecord(t, lines[0], f.injected[0], msgID, true, f.log, "ratchet_epoch")

	// THE MESSAGE ITSELF SURVIVED. It was applied before the idem payload was
	// even looked at, and it must stay applied.
	hubfuAssertServed(t, f, msgID, body)

	// The fault is the applied-key record, not the message: neither
	// message-record discard line may fire.
	hubfuAssertAbsent(t, f, hubfuApplyDiscardLine, hubfuDecodeDiscardLine, hubfuIdemRememberDiscardLine, hubfuIdemRebuildDiscardLine)
}

// ---------------------------------------------------------------------------
// hub.go, Apply: an applied-key record that cannot be REMEMBERED
// ---------------------------------------------------------------------------

// TestPreIdem11AppliedKeyThatCannotBeRememberedIsDiscardedLoudly drives Apply's
// idem.Recover error branch, through the PRE-IDEM-11 rebuild path.
//
// # THE TRIGGER, AND WHY IT IS NOT SYNTHETIC
//
// An entry with NO idem payload is what every record written before IDEM-11
// looks like, so the applied-key record is REBUILT from the message's own
// durable fields — including its idempotency key. store.Decode bounds that key
// by LENGTH only; idem.ValidateKey additionally enforces KeyCharset. A durable
// record carrying a key outside that charset therefore produces a valid message
// and an unrememberable applied-key record. Both halves of that asymmetry are
// PROVEN before the fixture is trusted (hubfuAssertRememberPremise), because a
// fixture that quietly stopped reaching this branch would leave the test green
// and testing nothing — the ACK-4 shape.
//
// # THIS ALSO PROVES THE REBUILD PATH IS LIVE
//
// Reaching idem.Recover at all means recoverIdemRecord took the back-compat
// arm, rebuilt the record from the message and called encodeStoredResult
// successfully. That is the surrounding path of the one discard line in Apply
// that has no reachable trigger (hubfuUnreachableRebuildNote).
func TestPreIdem11AppliedKeyThatCannotBeRememberedIsDiscardedLoudly(t *testing.T) {
	hubfuAssertRememberPremise(t)

	const body = "the pre-IDEM-11 message whose key this build will not remember"
	var msgID string
	f := hubfuRecover(t, func(t *testing.T, alpha, beta string) ([]wal.Entry, uint64) {
		t.Helper()
		rec, id := hubfuMessageRecord(t, hubfuBaseSeq, alpha, beta, body, hubfuUnacceptableKey)
		msgID = id
		// NO Idem payload: that absence is what selects the pre-IDEM-11 rebuild
		// path in recoverIdemRecord.
		return []wal.Entry{{Kind: store.RecordKind, Body: rec}}, hubfuBaseSeq
	})
	hubfuAssertRunning(t, f)

	lines := relay52Select(t, f.log, hubfuIdemRememberDiscardLine)
	if len(lines) != 1 {
		t.Fatalf("one pre-IDEM-11 record carried an idempotency key this build's validator refuses, and %d %q line(s) were emitted, want exactly 1. The applied-key memory for that key is gone and nothing else in recovery reports it, so without this line a retry silently becomes a SECOND message (invariant 10).\nlog: %s", len(lines), hubfuIdemRememberDiscardLine, f.log)
	}
	// This line carries no commit_index — it is emitted from Apply, which
	// reports the prepare index and the message. hubfuAssertNamesIdemRecord is
	// told so explicitly rather than guessing, so a field that DISAPPEARS is a
	// failure rather than a silently relaxed assertion.
	// "byte " is the DISTINGUISHING half. "idempotency key" alone is
	// ErrInvalidKey's own sentinel text, so logging the bare sentinel and
	// dropping the offending byte index SURVIVED (reviewer N8) — and that is
	// exactly what a well-meant "stop echoing untrusted bytes" change would
	// produce. ValidateKey already quotes only the single offending BYTE and
	// refuses to echo an oversized key at all, so requiring it asks for nothing
	// unbounded.
	hubfuAssertNamesIdemRecord(t, lines[0], f.injected[0], msgID, false, f.log, "idempotency key", "byte ")

	// Same blast-radius rule as the decode branch: the MESSAGE is fine.
	hubfuAssertServed(t, f, msgID, body)
	hubfuAssertAbsent(t, f, hubfuApplyDiscardLine, hubfuDecodeDiscardLine, hubfuIdemDecodeDiscardLine, hubfuIdemRebuildDiscardLine)
}

// hubfuAssertRememberPremise proves the fixture key really does sit in the gap
// between the two validators. If either half stops holding — store tightens, or
// idem loosens — the fixture stops reaching Apply's idem.Recover error branch,
// and this says so instead of passing.
func hubfuAssertRememberPremise(t *testing.T) {
	t.Helper()
	if err := idem.ValidateKey(hubfuUnacceptableKey); err == nil {
		t.Fatalf("PREMISE BROKEN: idem.ValidateKey now ACCEPTS %q, so the rebuilt applied-key record would be remembered and the %q branch would never be reached. This test would then pass while asserting nothing about it.", hubfuUnacceptableKey, hubfuIdemRememberDiscardLine)
	}
	if n := len(hubfuUnacceptableKey); n > store.MaxIdempotencyKeyLen {
		t.Fatalf("PREMISE BROKEN: the fixture key is %d bytes and store.MaxIdempotencyKeyLen is %d, so store.Decode would refuse the whole record and the fixture would drive the DECODE discard instead of the remember discard.", n, store.MaxIdempotencyKeyLen)
	}
}

// ---------------------------------------------------------------------------
// The one-word collapse
// ---------------------------------------------------------------------------

// TestMessageRecordDiscardLinesAreNotInterchangeable pins the distinction
// between the DECODED and the APPLIED message-record discard lines from both
// sides.
//
// The two sentences differ by one word and describe different faults: "could
// not be decoded" means this build does not understand the bytes — a schema
// bump, or damaged media — and is counted and summarised at the end of
// recovery; "could not be applied" means the bytes were understood and the
// SERVING COPY refused them, which is an invariant-1 problem upstream and is
// counted nowhere. Telling an operator the wrong one sends them to the wrong
// diagnosis, and a substring-matching test cannot tell them apart at all.
//
// Each side drives ONE fault and asserts the OTHER line is absent, so
// collapsing either wording onto the other's is red in both directions.
func TestMessageRecordDiscardLinesAreNotInterchangeable(t *testing.T) {
	// DOCUMENTATION, NOT A RUNTIME ASSERTION, and worth being explicit about:
	// both operands are constants in THIS file, so the compiler knows the answer
	// and this can never fire at run time. It guards the file's own constants
	// against a careless copy-paste that makes them equal — the production lines
	// are pinned by the two subtests below, which DO run.
	if hubfuApplyDiscardLine == hubfuDecodeDiscardLine {
		t.Fatalf("the two message-record discard lines are now the SAME string in this test file; they report different faults with different follow-up, and one wording for both is not a tidy-up.")
	}

	t.Run("a record this build cannot decode reports DECODED and not APPLIED", func(t *testing.T) {
		f := hubfuRecover(t, func(t *testing.T, alpha, beta string) ([]wal.Entry, uint64) {
			t.Helper()
			// NOTHING is applied: the record is discarded, so this fixture
			// establishes no floor beyond the accepted message's own sequence.
			return []wal.Entry{{Kind: store.RecordKind, Body: hubfuUndecodableRecord()}}, 0
		})
		hubfuAssertRunning(t, f)
		if n := len(relay52Select(t, f.log, hubfuDecodeDiscardLine)); n != 1 {
			t.Fatalf("an undecodable record produced %d %q line(s), want 1.\nlog: %s", n, hubfuDecodeDiscardLine, f.log)
		}
		hubfuAssertAbsent(t, f, hubfuApplyDiscardLine)
	})

	t.Run("a record the serving copy refuses reports APPLIED and not DECODED", func(t *testing.T) {
		f := hubfuRecover(t, func(t *testing.T, alpha, beta string) ([]wal.Entry, uint64) {
			t.Helper()
			body, _ := hubfuMessageRecord(t, hubfuBaseSeq, alpha, beta, "present twice", "k-relay52fu-pin")
			return []wal.Entry{
				{Kind: store.RecordKind, Body: body},
				{Kind: store.RecordKind, Body: body},
			}, hubfuBaseSeq
		})
		hubfuAssertRunning(t, f)
		if n := len(relay52Select(t, f.log, hubfuApplyDiscardLine)); n != 1 {
			t.Fatalf("a duplicated record produced %d %q line(s), want 1.\nlog: %s", n, hubfuApplyDiscardLine, f.log)
		}
		hubfuAssertAbsent(t, f, hubfuDecodeDiscardLine)
	})
}

// ---------------------------------------------------------------------------
// Shared assertions
// ---------------------------------------------------------------------------

// hubfuAssertRunning checks the OTHER half of invariant 6: the bus reached a
// running state.
//
// "hub.Open returned no error" is deliberately not the assertion — a hub that
// opened poisoned, or opened and served nothing, satisfies that and satisfies
// nothing an operator cares about. This exercises all three things the bus must
// still be able to do after a discard: it is not poisoned, it still SERVES the
// history that survived, and it still ACCEPTS new work at a sequence above the
// one already burned (invariant 1 — a discard advances past the hole, it never
// rewinds).
func hubfuAssertRunning(t *testing.T, f hubfuRecovered) {
	t.Helper()
	if err := f.h.Poisoned(); err != nil {
		t.Fatalf("the bus started but is POISONED after discarding a record during recovery: %v\nA discard is sanctioned by invariant 6; refusing to serve afterwards is the outage it exists to avoid.\nlog: %s", err, f.log)
	}
	hubfuAssertServed(t, f, f.good.MessageID, hubfuGoodBody)

	// THE FLOOR IS THE HIGHEST SEQUENCE ANY REPLAYED RECORD PROVES WAS WRITTEN,
	// not merely the one the accepted message carries.
	//
	// Checking only against f.good.Seq (1 in every fixture here) would be
	// satisfied by almost any mint and would assert nothing: the fixtures that
	// APPLY a hand-built record at hubfuBaseSeq establish a floor three orders of
	// magnitude higher, and it is that floor invariant 1 is about — "never resume
	// at or below a sequence a replayed message record proves was written"
	// (hub.Open's mint-floor source (3)). A recovered bus that re-issued a number
	// already burned by a replayed record would hand a second message the identity
	// of one already on disk, which is the never-reuse rule broken in the one
	// place it is hardest to notice: only after a restart.
	floor := f.good.Seq
	if f.appliedSeq > floor {
		floor = f.appliedSeq
		// AND THE FIXTURE MUST STILL DISCRIMINATE THE SOURCE IT IS AIMED AT.
		//
		// hub.Open derives the mint floor from several sources; one of them is
		// the WAL HIGH-WATER INDEX, which rises with the number of records
		// written. The assertion below is aimed at a DIFFERENT source — "never
		// resume at or below a sequence a replayed message record proves was
		// written". If the WAL index ever climbed past the sequence this fixture
		// applies at, the mint would clear the bar on the index alone and
		// deleting the replayed-sequence source would stop being observable —
		// SILENTLY, with everything still green. That is not hypothetical
		// arithmetic: the index reached 257 here while the fixture applied at
		// 1000, so the margin is real but it is a margin, not a guarantee.
		var highestIndex uint64
		for _, c := range f.injected {
			if c.CommitIndex > highestIndex {
				highestIndex = c.CommitIndex
			}
		}
		if highestIndex >= f.appliedSeq {
			t.Fatalf("this fixture writes WAL records up to commit index %d but applies its message record at sequence %d. The mint floor is the MAXIMUM over several sources, one of which is the WAL high-water index, so at these numbers the recovered mint clears %d on the index alone and this assertion no longer proves anything about the REPLAYED-SEQUENCE source it is aimed at.\n\nRAISE hubfuBaseSeq well above the number of records the fixtures write.\nlog: %s", highestIndex, f.appliedSeq, f.appliedSeq, f.log)
		}
	}
	m := mustMint(t, f.h, f.alpha, "send", "k-relay52fu-after-recovery")
	if m.Seq <= floor {
		t.Fatalf("after recovery the bus minted sequence %d, at or below the %d a replayed record proves was already written (the accepted message carries %d; the fixture applied a record at %d). A discard must never rewind the sequence, and a number already burned must never be handed out again (invariant 1).\nlog: %s", m.Seq, floor, f.good.Seq, f.appliedSeq, f.log)
	}
}

// hubfuAssertServed checks that beta can still read a message back, by id and
// by body. It is the "damaged records are discarded, the history around them is
// NOT" half of invariants 4 and 6.
func hubfuAssertServed(t *testing.T, f hubfuRecovered, wantID, wantBody string) {
	t.Helper()
	batch := mustHistory(t, f.h, f.beta, 0, hub.MaxBatchLimit)
	for _, m := range batch.Messages {
		if m.ID != wantID {
			continue
		}
		if string(m.Body) != wantBody {
			t.Fatalf("message %s came back with body %q, want %q.\nlog: %s", m.ID, m.Body, wantBody, f.log)
		}
		return
	}
	t.Fatalf("after recovery the message %s is not in %s's history (%d message(s) served); the damaged record must be discarded, not the acknowledged history around it (invariants 4 and 6).\nlog: %s", wantID, f.beta, len(batch.Messages), f.log)
}

// hubfuAssertNamesIdemRecord is the SPECIFICITY half for the two applied-key
// discard lines.
//
// withCommitIndex says whether this call site reports the commit index as well
// as the prepare index. It is a parameter rather than an "if the field is
// there" check on purpose: a field that vanishes must be a FAILURE, and an
// assertion that only fires when the field happens to be present cannot fail.
func hubfuAssertNamesIdemRecord(t *testing.T, fields map[string]string, c wal.Committed, wantMessageID string, withCommitIndex bool, log string, wantErrContains ...string) {
	t.Helper()
	if lvl := fields["level"]; lvl != "error" {
		t.Fatalf("the applied-key discard was logged at level=%q, want error; a lost idempotency guarantee reported below error is one an operator's filters drop, and invariant 10's duplicate suppression is exactly what is gone.\nlog: %s", lvl, log)
	}
	want := []struct {
		key string
		val string
	}{
		{"prepare_index", strconv.FormatUint(c.PrepareIndex, 10)},
		{"message_id", wantMessageID},
	}
	if withCommitIndex {
		want = append(want, struct {
			key string
			val string
		}{"commit_index", strconv.FormatUint(c.CommitIndex, 10)})
	}
	for _, w := range want {
		if got := relay52Field(t, fields, w.key, log); got != w.val {
			t.Fatalf("the applied-key discard line reports %s=%s, want %s. Without the WAL index and the message it belongs to, an operator cannot tell WHICH key stopped being suppressed, and \"a key was lost\" is not an actionable report.\nlog: %s", w.key, got, w.val, log)
		}
	}
	// As on the apply line, non-empty is NOT enough — see hubfuAssertErrExplains.
	// The caller says which substance identifies its fault: the unknown FIELD NAME
	// for a record a newer build wrote, the words "idempotency key" for a key this
	// build's validator refuses.
	hubfuAssertErrExplains(t, fields, log, wantErrContains...)
}

// hubfuAssertAbsent checks that a fault drove ONLY its own discard line. Every
// line in this family reports a different fault with different follow-up, so a
// second one firing is a misreport, not extra detail.
func hubfuAssertAbsent(t *testing.T, f hubfuRecovered, lines ...string) {
	t.Helper()
	for _, want := range lines {
		if n := len(relay52Select(t, f.log, want)); n != 0 {
			t.Fatalf("the fixture drove ONE fault but %q was emitted %d time(s) as well. These lines name different faults and send an operator to different diagnoses; emitting more than one for a single fault means at least one of them is being reported on the wrong branch.\nlog: %s", want, n, f.log)
		}
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// hubfuMessageRecord builds a durable message record the way the live write
// path builds one, and returns its bytes and the message id it carries.
//
// It goes through store.Message.Encode rather than assembling JSON by hand so
// the record is exactly the shape this build writes: a fixture that hand-rolled
// the JSON would drift from the real record the first time a field moved, and
// would then be testing store.Decode's tolerance rather than Hub.Apply's
// behaviour.
func hubfuMessageRecord(t *testing.T, seq uint64, sender, recipient, body, idemKey string) (json.RawMessage, string) {
	t.Helper()
	id, err := ids.MessageID(testBusID, seq)
	if err != nil {
		t.Fatalf("ids.MessageID(%q, %d): %v", testBusID, seq, err)
	}
	m := store.Message{
		Seq:        seq,
		ID:         id,
		Sender:     sender,
		Recipients: []string{recipient},
		// This bus's clock, and it must be AFTER the roster's enrolment instant
		// or the recovered message is invisible to every reader and the
		// "it is still served" assertions go vacuous.
		SentAt:             time.Now().UTC(),
		Body:               []byte(body),
		ContentSHA256:      store.ContentHash([]byte(body)),
		IdempotencyKey:     idemKey,
		TimestampUnixMilli: fixtureTimestampMs,
		Signature:          fixtureSignature(),
	}
	raw, err := m.Encode()
	if err != nil {
		t.Fatalf("encoding the hand-built message record for sequence %d: %v", seq, err)
	}
	// PREMISE: this build must be able to DECODE what the fixture just wrote.
	// Without this a record that had quietly become undecodable would drive the
	// decode branch while the test believed it was driving the apply branch.
	if _, err := store.Decode(raw); err != nil {
		t.Fatalf("PREMISE BROKEN: the hand-built record for sequence %d does not decode (%v), so it would drive the UNDECODABLE-record branch instead of the branch under test.", seq, err)
	}
	return raw, id
}

// hubfuUndecodableRecord is a record this build cannot decode: valid JSON, a
// valid WAL body, and a record schema version store.Decode refuses on its
// exact-version check. It models the first start after a record-schema bump.
func hubfuUndecodableRecord() json.RawMessage { return json.RawMessage(`{"v":99}`) }

// hubfuFutureIdemRecord is an applied-key record this build cannot decode.
//
// It is the EXACT bytes this build would write for a valid record, plus one
// field a future build added. idem.DecodeRecord uses DisallowUnknownFields, so
// that is precisely the forward-compatibility hazard wal.Entry.Idem's own doc
// records — not synthetic garbage, which would exercise the JSON parser instead
// of the record decoder.
//
// Both halves are PROVEN here: the unmodified record decodes, and the modified
// one does not. If the extra field ever became known, this would say so rather
// than silently stop reaching the branch.
func hubfuFutureIdemRecord(t *testing.T, agent, key string) json.RawMessage {
	t.Helper()
	valid, err := idem.Record{
		Agent:       agent,
		Op:          idem.OpSend,
		Key:         key,
		Result:      json.RawMessage(`{"ok":true}`),
		Seq:         hubfuBaseSeq,
		CommittedAt: time.Now().UTC(),
	}.Encode()
	if err != nil {
		t.Fatalf("encoding the baseline applied-key record: %v", err)
	}
	if _, err := idem.DecodeRecord(valid); err != nil {
		t.Fatalf("PREMISE BROKEN: the baseline applied-key record does not decode (%v), so adding a field to it would not be what makes it undecodable and this fixture would prove nothing about the unknown-field path.", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(valid, &fields); err != nil {
		t.Fatalf("unmarshalling the baseline applied-key record: %v", err)
	}
	fields["ratchet_epoch"] = json.RawMessage(`3`)
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshalling the future applied-key record: %v", err)
	}
	if _, err := idem.DecodeRecord(out); err == nil {
		t.Fatalf("PREMISE BROKEN: idem.DecodeRecord now ACCEPTS a record carrying an unknown field, so this fixture would be applied rather than discarded and the %q branch would never be reached.", hubfuIdemDecodeDiscardLine)
	}
	return json.RawMessage(out)
}

// hubfuMeasureDecodeCap measures how many undecodable message records THIS
// BUILD names individually before the one-shot cap notice fires.
//
// The cap is unexported, so it is derived from the log rather than restated as
// a literal: this file's claim is that the APPLY branch is not subject to it,
// and that claim has to keep meaning the same thing when the constant changes.
//
// THE PROBE MUST PROVE THE CAP FIRED. An earlier version returned
// min(cap, probe) and merely claimed that "stays honest either way" — it does
// not, it stays non-FALSE. The reviewer raised maxDecodeFailuresLogged from 8 to
// 40 in an overlay: 24 < 40, so no cap engaged, the measurement silently became
// "24", the apply branch then injected 27, and the whole file still passed while
// the claim it exists to prove — that the apply branch is not subject to the
// decode cap — had stopped being proved. That is exactly the ACK-4 shape (a
// fixture too short to reach its own boundary), so the cap notice is now a hard
// requirement and a too-small probe FAILS LOUDLY asking to be raised.
func hubfuMeasureDecodeCap(t *testing.T) int {
	t.Helper()
	const probe = 24
	f := hubfuRecover(t, func(t *testing.T, alpha, beta string) ([]wal.Entry, uint64) {
		t.Helper()
		out := make([]wal.Entry, 0, probe)
		for i := 0; i < probe; i++ {
			out = append(out, wal.Entry{Kind: store.RecordKind, Body: hubfuUndecodableRecord()})
		}
		// Every one of them is discarded, so none establishes a floor.
		//
		// NOTE, so the next reader does not trust this number: it is the ONE
		// declaration in this file that is never checked, because the probe does
		// not call hubfuAssertRunning. It is correct, but nothing here would
		// catch it if it stopped being — unlike the other five, where an
		// over-declaration goes red at the mint assertion.
		return out, 0
	})
	n := len(relay52Select(t, f.log, hubfuDecodeDiscardLine))
	if n == 0 {
		t.Fatalf("%d undecodable records produced NO %q line at all, so no cap could be measured and the discard is SILENT (invariant 6).\nlog: %s", probe, hubfuDecodeDiscardLine, f.log)
	}
	if n > probe {
		t.Fatalf("%d undecodable records produced %d individual discard lines; more lines than records means the branch is being reached from somewhere this fixture does not control.\nlog: %s", probe, n, f.log)
	}
	// THE CAP MUST HAVE ENGAGED, or this is not a measurement of a cap.
	if caps := relay52Select(t, f.log, hubfuDecodeCapLine); len(caps) != 1 {
		t.Fatalf("the probe injected %d undecodable records and saw the cap-reached line %d time(s), want exactly 1: the per-record cap did NOT engage, so %d is not the cap — it is just how many records this probe happened to write.\n\nRAISE THE PROBE above the current value of maxDecodeFailuresLogged and re-run. Returning this number anyway is how the caller's \"the apply branch is not subject to the decode cap\" claim silently stops being proved while every test still passes.\nlog: %s", probe, len(caps), n, f.log)
	}
	if n >= probe {
		t.Fatalf("the probe injected %d undecodable records and %d were logged individually; the measurement must be strictly below the probe or the probe is not big enough to have found the cap. RAISE THE PROBE.\nlog: %s", probe, n, f.log)
	}
	return n
}
