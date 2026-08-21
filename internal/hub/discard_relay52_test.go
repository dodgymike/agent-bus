package hub_test

// INVARIANT 6, THE LOUD HALF, ON THE HUB RECOVERY PLANE (RELAY-52).
//
// Invariant 6 makes a two-part promise about a record recovery cannot
// understand:
//
//	recovery ALWAYS reaches a running server, damaged records are discarded,
//	and every discard is logged LOUDLY AND SPECIFICALLY — silent discard is
//	the defect, not discard itself.
//
// Hub.Apply's decode-error branch is where that promise is kept for a MESSAGE
// record. Before this file nothing in the tree asserted either half of it: the
// only test naming a hub discard line (outoforder_poison_sign1fu_test.go)
// asserts a DIFFERENT line is ABSENT, so deleting the log call here, or hollowing
// it out until it named no record, left the whole suite green.
//
// # THE TRIGGER IS THE REAL ONE, NOT SYNTHETIC GARBAGE
//
// The fixture appends records that are valid JSON, valid WAL entries and valid
// store.RecordKind — and carry a record schema version this build does not
// understand. That is exactly the case Apply's comment cites: the first start
// after a store record-schema bump, when EVERY message record in the log is
// refused by store.Decode. Garbage bytes would exercise the JSON decoder
// instead and would not reach the branch by the route production reaches it.
//
// # THE CAP IS THE TRAP
//
// The per-record line is capped (hub.go: maxDecodeFailuresLogged, unexported),
// so a fixture with one bad record can only ever see the below-cap branch and a
// test that stopped there would claim to cover a boundary it never reached. Both
// branches therefore get their own fixture, and the past-cap one asserts the
// cap's SHAPE — count, one-shot line, self-consistency with the logged_up_to it
// reports, and the TRUE total in the end-of-recovery summary — rather than the
// constant's value, which is not exported and must be free to change without
// making this test lie.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// The three lines under test, quoted in full. They are separate constants from
// outoforder_poison_sign1fu_test.go's discardOnApplyLine, which names the
// near-identical "could not be APPLIED" line at a different call site: the two
// differ by one word and collapsing them would let a test pass on the wrong one.
const (
	// Emitted per undecodable record, up to the cap (hub.go, Apply).
	relay52DecodeDiscardLine = "DISCARDING a message record that could not be decoded during recovery; it is not in this bus's history and will not be delivered"
	// Emitted ONCE, when the cap is reached (hub.go, Apply).
	relay52DecodeCapLine = "further undecodable message records will NOT be logged individually; the total is reported once recovery finishes"
	// The end-of-recovery summary (roster.go, noteRecoveredIdentities). Matched
	// by PREFIX: the full sentence is a paragraph of operator guidance and
	// pinning every word of it here would make this test a proofreader.
	relay52SummaryPrefix = "THE AGENT-ID REUSE CHECK RAN ON INCOMPLETE INPUT"
)

// relay52BadRecord is a record this build cannot decode. store.Decode requires
// an EXACT match on the schema version (internal/store/message.go), so v99
// models a log written by a future build — or, read the other way round, every
// v1 record in an existing data directory on the first start after a bump.
var relay52BadRecord = json.RawMessage(`{"v":99}`)

const relay52GoodBody = "the one message that really was accepted"

// relay52Recovered is a bus recovered over a log holding one genuinely accepted
// message followed by junk undecodable records.
type relay52Recovered struct {
	h *hub.Hub
	// log is everything the hub logged from Open onwards.
	log string
	// injected is what wal.Write reported for each undecodable record, so the
	// test can assert the LOGGED indices are the real ones and not a constant.
	injected []wal.Committed
	// good identifies the message that must still be in history afterwards.
	good hub.Result
	// alpha sends, beta receives.
	alpha, beta string
}

// relay52Recover builds the fixture and reopens the bus over it.
//
// The accepted history comes FIRST and is written by the hub itself, through
// Mint and Send, so recovery has something legitimate to rebuild and the
// assertions about a running bus are about a bus that really did recover state —
// not about an empty log that could not have lost anything.
func relay52Recover(t *testing.T, undecodable int) relay52Recovered {
	t.Helper()
	if undecodable < 1 {
		t.Fatalf("relay52Recover(%d): the fixture must inject at least one undecodable record", undecodable)
	}
	dir := t.TempDir()
	alpha := agentID(t, testBusID, "alpha")
	beta := agentID(t, testBusID, "beta")

	// (1) A REAL accepted history: minted, sent, committed and fsynced.
	lg := openTestLog(t, dir, false)
	h, _ := openMintHub(t, dir, lg, nil, "", "alpha", "beta")
	good, err := mintedSend(t, h, hub.SendRequest{
		Sender:         alpha,
		To:             beta,
		Body:           []byte(relay52GoodBody),
		IdempotencyKey: "k-relay52-good",
	})
	if err != nil {
		t.Fatalf("building the accepted history: Send: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("building the accepted history: closing the log: %v", err)
	}

	// (2) The damage, appended DIRECTLY to the durable log — the hub would never
	// write one of these, which is the point: they model records written by a
	// build whose record schema this one does not understand.
	lg2 := openTestLog(t, dir, false)
	injected := make([]wal.Committed, 0, undecodable)
	for i := 0; i < undecodable; i++ {
		c, err := lg2.Write(wal.Entry{Kind: store.RecordKind, Body: relay52BadRecord})
		if err != nil {
			t.Fatalf("appending undecodable record %d: %v", i, err)
		}
		injected = append(injected, c)
	}
	if err := lg2.Close(); err != nil {
		t.Fatalf("closing the log after appending the undecodable records: %v", err)
	}

	// (3) THE RESTART. Replay drives Hub.Apply over the whole log.
	lg3 := openTestLog(t, dir, true)
	h2, buf, err := tryOpenMintHub(t, dir, lg3, nil, "", "alpha", "beta")
	if err != nil {
		t.Fatalf("hub.Open over a log holding %d undecodable message record(s) REFUSED TO START: %v\n\nInvariant 6 settled this trade: recovery ALWAYS reaches a running server and the damaged records are discarded loudly. Failing closed here turns one unreadable record into an outage, and an operator has no way to get the bus back.\nlog: %s", undecodable, err, buf.String())
	}
	return relay52Recovered{h: h2, log: buf.String(), injected: injected, good: good, alpha: alpha, beta: beta}
}

func TestUndecodableMessageRecordIsDiscardedLoudlyAndTheBusStarts(t *testing.T) {
	// BELOW THE CAP. One bad record: the per-record line is emitted, and the
	// one-shot cap line must NOT appear.
	t.Run("one undecodable record is named individually and no cap line is emitted", func(t *testing.T) {
		f := relay52Recover(t, 1)
		relay52AssertRunning(t, f)

		lines := relay52Select(t, f.log, relay52DecodeDiscardLine)
		if len(lines) != 1 {
			t.Fatalf("one undecodable record produced %d %q lines, want exactly 1; invariant 6 permits the discard and forbids it being silent.\nlog: %s", len(lines), relay52DecodeDiscardLine, f.log)
		}
		relay52AssertNamesRecord(t, lines[0], f.injected[0], 1, f.log)
		if caps := relay52Select(t, f.log, relay52DecodeCapLine); len(caps) != 0 {
			t.Fatalf("a single discard emitted the cap-reached line %d time(s); it must appear only once the per-record cap is actually reached, or an operator reads \"further records will not be logged\" while every record WAS logged.\nlog: %s", len(caps), f.log)
		}
		relay52AssertSummaryTotal(t, f, 1)
	})

	// PAST THE CAP. Twelve bad records — comfortably more than
	// maxDecodeFailuresLogged (8 at the time of writing, unexported and free to
	// change). Every assertion below derives the cap from the log itself.
	t.Run("past the per-record cap the flood is capped, counted, and the true total still reported", func(t *testing.T) {
		const injected = 12
		f := relay52Recover(t, injected)
		relay52AssertRunning(t, f)

		lines := relay52Select(t, f.log, relay52DecodeDiscardLine)
		if len(lines) == 0 {
			t.Fatalf("%d undecodable records produced NO %q line at all.\nlog: %s", injected, relay52DecodeDiscardLine, f.log)
		}
		if len(lines) >= injected {
			t.Fatalf("%d undecodable records produced %d individual discard lines; the cap did not engage, so this fixture proves nothing about the capped branch and a schema bump would emit one ERROR per message on exactly the start an operator most needs to read the log.\nlog: %s", injected, len(lines), f.log)
		}
		// The lines that WERE emitted are the first N in commit order, each
		// naming its own record and its own running count.
		for i, fields := range lines {
			relay52AssertNamesRecord(t, fields, f.injected[i], i+1, f.log)
		}

		caps := relay52Select(t, f.log, relay52DecodeCapLine)
		if len(caps) != 1 {
			t.Fatalf("the cap-reached line appeared %d times, want exactly 1: it is a ONE-SHOT notice, and repeating it is the same flood it exists to prevent.\nlog: %s", len(caps), f.log)
		}
		// SELF-CONSISTENCY: the cap line tells the operator how far the
		// individual logging got. If that number and the count of lines actually
		// emitted ever disagree, the operator is being told the wrong place to
		// stop reading.
		got := relay52Field(t, caps[0], "logged_up_to", f.log)
		if want := strconv.Itoa(len(lines)); got != want {
			t.Fatalf("the cap-reached line says logged_up_to=%s but %s individual discard lines were emitted; the notice must describe what actually happened.\nlog: %s", got, want, f.log)
		}
		// THE TRUE TOTAL IS NOT LOST. This is the whole justification for
		// capping: the exact count survives to the end-of-recovery summary.
		relay52AssertSummaryTotal(t, f, injected)
	})
}

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

// relay52AssertRunning checks the OTHER half of invariant 6: the bus reached a
// running state.
//
// # THE MUTATION THAT DOES NOT BITE, RECORDED SO THE NEXT READER DOES NOT REPEAT IT
//
// Returning the decode error from Apply instead of nil is NOT observable here,
// and that is a property of the layer below rather than a weakness of this
// test: internal/wal's replay demotes an applier rejection to a discard of its
// own and carries on (internal/wal/replay.go, the fn(c) error branch), so the
// bus starts either way. Apply's `return nil` is defence in depth over that
// policy, not the thing that keeps it. The reachable ways to lose the
// running-state guarantee are Open failing CLOSED once anything was discarded,
// and Open succeeding but POISONED — both of which the assertions below catch.
//
// "hub.Open returned no error" is deliberately NOT the assertion. A hub that
// opened and then served nothing, or that opened poisoned, satisfies that and
// satisfies nothing an operator cares about — so this exercises all three things
// the bus must still be able to do: it is not poisoned, it still SERVES the
// history that survived, and it still ACCEPTS new work.
func relay52AssertRunning(t *testing.T, f relay52Recovered) {
	t.Helper()
	if err := f.h.Poisoned(); err != nil {
		t.Fatalf("the bus started but is POISONED after discarding undecodable records: %v\nA discard is sanctioned by invariant 6; refusing to serve afterwards is the outage it exists to avoid.\nlog: %s", err, f.log)
	}
	batch := mustHistory(t, f.h, f.beta, 0, hub.MaxBatchLimit)
	found := false
	for _, m := range batch.Messages {
		if m.ID == f.good.MessageID {
			found = true
			if string(m.Body) != relay52GoodBody {
				t.Fatalf("the surviving message %s came back with body %q, want %q", m.ID, m.Body, relay52GoodBody)
			}
		}
	}
	if !found {
		t.Fatalf("after recovery the ACCEPTED message %s is not in %s's history (%d message(s) served); the undecodable records must be discarded, not the acknowledged history around them (invariants 4 and 6).\nlog: %s", f.good.MessageID, f.beta, len(batch.Messages), f.log)
	}
	// New work, on the plane invariant 1 governs: the mint must still hand out a
	// number, and it must be above the sequence the surviving message already
	// burned.
	m := mustMint(t, f.h, f.alpha, "send", "k-relay52-after-recovery")
	if m.Seq <= f.good.Seq {
		t.Fatalf("after recovery the bus minted sequence %d, at or below the %d the surviving message already carries; a discard must never rewind the sequence (invariant 1).\nlog: %s", m.Seq, f.good.Seq, f.log)
	}
}

// relay52AssertNamesRecord is the SPECIFICITY half of "loud and specific": the
// line must let an operator find the record it is about.
//
// It asserts the FIELDS, parsed out of the record, against the indices wal
// actually reported for the injected entry. A substring check on the message
// text would pass just as happily against a line that named no record at all,
// which is the failure mode that makes a discard effectively silent even though
// something was printed.
func relay52AssertNamesRecord(t *testing.T, fields map[string]string, c wal.Committed, wantSoFar int, log string) {
	t.Helper()
	if lvl := fields["level"]; lvl != "error" {
		t.Fatalf("the discard was logged at level=%q, want error; invariant 6 requires it to be LOUD, and a discard reported below error is one an operator's filters drop.\nlog: %s", lvl, log)
	}
	for _, want := range []struct {
		key string
		val string
	}{
		{"prepare_index", strconv.FormatUint(c.PrepareIndex, 10)},
		{"commit_index", strconv.FormatUint(c.CommitIndex, 10)},
		{"discarded_so_far", strconv.Itoa(wantSoFar)},
	} {
		if got := relay52Field(t, fields, want.key, log); got != want.val {
			t.Fatalf("the discard line reports %s=%s, want %s. The line must name the record that was thrown away — without its WAL indices an operator cannot locate it, and \"a record was discarded\" is not an actionable report.\nlog: %s", want.key, got, want.val, log)
		}
	}
	// The REASON, in the operator's hands. It is the store decoder's own error,
	// which is what tells a schema bump apart from damaged media.
	if e := relay52Field(t, fields, "err", log); strings.TrimSpace(e) == "" {
		t.Fatalf("the discard line carries an empty err field; the reason a record could not be decoded is the difference between \"upgrade expected this\" and \"this disk is damaged\".\nlog: %s", log)
	}
}

// relay52AssertSummaryTotal pins the end-of-recovery summary to the TRUE number
// of discarded records — capped logging must never cap the count.
func relay52AssertSummaryTotal(t *testing.T, f relay52Recovered, want int) {
	t.Helper()
	var summary map[string]string
	for _, fields := range relay52Records(t, f.log) {
		if strings.HasPrefix(fields["msg"], relay52SummaryPrefix) {
			if summary != nil {
				t.Fatalf("the end-of-recovery summary was emitted more than once.\nlog: %s", f.log)
			}
			summary = fields
		}
	}
	if summary == nil {
		t.Fatalf("recovery discarded %d message record(s) and never emitted the %q summary; the id-reuse check then reports a clean result having examined nothing, and reads as an all-clear.\nlog: %s", want, relay52SummaryPrefix, f.log)
	}
	if got, w := relay52Field(t, summary, "undecodable_message_records", f.log), strconv.Itoa(want); got != w {
		t.Fatalf("the end-of-recovery summary reports undecodable_message_records=%s, want %s. The per-record LINES are capped; the COUNT must not be, or the cap silently rewrites how much history was lost.\nlog: %s", got, w, f.log)
	}
}

// ---------------------------------------------------------------------------
// logfmt
// ---------------------------------------------------------------------------

// relay52Records parses every line the logger emitted into its key/value pairs.
func relay52Records(t *testing.T, log string) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, line := range strings.Split(log, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, relay52ParseLine(t, line))
	}
	return out
}

// relay52Select returns the parsed records whose msg is exactly want, in the
// order they were logged.
func relay52Select(t *testing.T, log, want string) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, fields := range relay52Records(t, log) {
		if fields["msg"] == want {
			out = append(out, fields)
		}
	}
	return out
}

// relay52Field reads one field and fails with the whole record if it is absent,
// so a stripped field reports as the missing field rather than as an empty
// string that compares equal to nothing in particular.
func relay52Field(t *testing.T, fields map[string]string, key, log string) string {
	t.Helper()
	v, ok := fields[key]
	if !ok {
		t.Fatalf("the log record %s has no %q field.\nlog: %s", relay52Format(fields), key, log)
	}
	return v
}

func relay52Format(fields map[string]string) string {
	return fmt.Sprintf("%v", fields)
}

// relay52ParseLine parses one logfmt line: bare values, and values quoted with
// strconv.Quote (which is what internal/logging emits for anything containing a
// space, a quote, an '=' or a non-printable byte).
func relay52ParseLine(t *testing.T, line string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	i := 0
	for i < len(line) {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		if i >= len(line) {
			break
		}
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			t.Fatalf("log line is not logfmt at offset %d: %q", i, line)
		}
		key := line[i : i+eq]
		i += eq + 1
		if i < len(line) && line[i] == '"' {
			j := i + 1
			for j < len(line) {
				if line[j] == '\\' {
					j += 2
					continue
				}
				if line[j] == '"' {
					break
				}
				j++
			}
			if j >= len(line) {
				t.Fatalf("log line has an unterminated quoted value for %q: %q", key, line)
			}
			v, err := strconv.Unquote(line[i : j+1])
			if err != nil {
				t.Fatalf("log line value for %q does not unquote (%v): %q", key, err, line)
			}
			out[key] = v
			i = j + 1
			continue
		}
		if j := strings.IndexByte(line[i:], ' '); j >= 0 {
			out[key] = line[i : i+j]
			i += j
		} else {
			out[key] = line[i:]
			i = len(line)
		}
	}
	return out
}
