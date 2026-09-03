package hub_test

// RELAY-52-FU-HUBDISCARDS-FU-APPLYDISCARD-UNCOUNTED.
//
// Apply's per-line discard log for a message record that DECODES but that
// store.Append refuses (a duplicate sequence) was already loud and specific
// before this task — discard_relay52fu_test.go proves that. What it was NOT was
// COUNTED: noteRecoveredIdentities' "THE AGENT-ID REUSE CHECK RAN ON INCOMPLETE
// INPUT" summary gated only on h.undecodableMessages, so an apply-discard left
// the id-reuse detector reporting a CLEAN result having silently skipped the
// discarded record's sender and recipient ids. Per invariant 6 a discard must be
// logged loudly AND counted; an aggregate signal that never counts it is the
// silent-discard defect expressed at the summary level.
//
// INVARIANTS READ IN FULL BEFORE WRITING THIS: 6 (recovery always reaches a
// running server; a discard is permitted, a SILENT discard is the defect — the
// summary is the aggregate signal an operator watches, so a discard no summary
// counts is silent at that level), 5 (memory is the serving copy, disk is the
// truth; the surviving history is still served).
//
// This test reuses discard_relay52fu_test.go's hubfuRecover / hubfuMessageRecord
// fixtures and the discard_relay52_test.go log helpers; it is a separate file so
// this production follow-up's proof does not entangle the (done) test-only task.

import (
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// TestApplyDiscardIsCountedTowardIncompleteInputSummary drives exactly one
// apply-branch discard and asserts the INCOMPLETE INPUT summary fires and counts
// it in its OWN field.
//
// # THE TRIGGER
//
// A message record present TWICE in the WAL: the first copy applies cleanly, the
// second is the duplicate sequence store.Append refuses — the one apply error
// replay can reach (see hubfuApplyDiscardLine's fixture in
// discard_relay52fu_test.go for why it is the only one).
//
// # RED BEFORE THE FIX
//
// Before this task the summary gated on h.undecodableMessages alone. This fixture
// drives zero undecodable records, so the summary was never emitted at all and
// the `summary == nil` branch below fired — the id-reuse detector reporting a
// clean result having examined the surviving records only.
func TestApplyDiscardIsCountedTowardIncompleteInputSummary(t *testing.T) {
	f := hubfuRecover(t, func(t *testing.T, alpha, beta string) ([]wal.Entry, uint64) {
		t.Helper()
		body, _ := hubfuMessageRecord(t, hubfuBaseSeq, alpha, beta, "present twice; the second copy is the apply-discard", "k-applydiscard-uncounted")
		return []wal.Entry{
			{Kind: store.RecordKind, Body: body},
			{Kind: store.RecordKind, Body: body},
		}, hubfuBaseSeq
	})
	hubfuAssertRunning(t, f)

	// The trigger really fired: exactly one apply-branch discard, and nothing
	// drove the DECODE branch (these records decoded cleanly).
	if n := len(relay52Select(t, f.log, hubfuApplyDiscardLine)); n != 1 {
		t.Fatalf("the fixture injected one duplicate message record but %d %q line(s) were emitted, want exactly 1; the counter assertions below would prove nothing if the apply-discard did not fire.\nlog: %s", n, hubfuApplyDiscardLine, f.log)
	}
	hubfuAssertAbsent(t, f, hubfuDecodeDiscardLine)

	// THE SUMMARY MUST FIRE. It is the aggregate signal an operator watches, and
	// its absence is exactly the silent-at-the-summary-level discard this task
	// closes (invariant 6).
	var summary map[string]string
	for _, fields := range relay52Records(t, f.log) {
		if strings.HasPrefix(fields["msg"], hubfuSummaryPrefix) {
			summary = fields
			break
		}
	}
	if summary == nil {
		t.Fatalf("an apply-branch discard occurred but no %q summary line was emitted; noteRecoveredIdentities then reports a CLEAN id-reuse result having silently skipped the discarded record's ids (invariant 6).\nlog: %s", hubfuSummaryPrefix, f.log)
	}

	// COUNTED IN ITS OWN FIELD. One record decoded and could not be applied.
	if got := relay52Field(t, summary, "unappliable_message_records", f.log); got != "1" {
		t.Fatalf("the summary reports unappliable_message_records=%s, want 1; one message record decoded but store.Append refused it, and this count is what makes the discard visible to the id-reuse check rather than silent.\nlog: %s", got, f.log)
	}

	// AND NOT MISCOUNTED AS UNDECODABLE. These records decoded; folding them into
	// undecodable_message_records would claim a record-schema problem that did not
	// happen and misdirect the operator. The reviewer blocked exactly that
	// mutation on the sibling task.
	if got := relay52Field(t, summary, "undecodable_message_records", f.log); got != "0" {
		t.Fatalf("the summary reports undecodable_message_records=%s, want 0; every record this fixture injected decoded cleanly and was refused by the serving copy, so nothing may be counted as undecodable.\nlog: %s", got, f.log)
	}
}
