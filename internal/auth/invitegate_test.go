package auth_test

// INVITE-GATE, part 2: the COMPOSITE record and the multiplexer that expands it.
//
// Three surfaces, and each one is load-bearing for a different reason:
//
//   - EncodeEnrolWithInvite / DecodeEnrolWithInvite. The on-disk shape is
//     FOREVER, so the round trip is pinned and the decoder's strictness is
//     pinned with it. A record read back off disk is UNTRUSTED INPUT even
//     though this server wrote it, because "this server wrote it" is exactly
//     the claim corruption disproves.
//   - MultiplexApplier. It is the log's ONE applier and it is what makes a
//     composite entry become two records. Its ERROR POLICY is the subtle part
//     and is asserted here directly: a composite it cannot decode returns NIL,
//     never an error, because an error poisons the log on a live write
//     (wal.ErrDiverged) and fails wal.Open on recovery — and invariant 6
//     settled that recovery ALWAYS reaches a running server. The absolute half
//     of that bargain is that every discard is LOGGED, specifically, so the log
//     line is asserted too and not merely the return value.
//   - WALRoster.PutWithInvite's `durable` flag. It is the bit a caller decides
//     an invite's fate by: abort a reservation whose consumption record is
//     already on disk and one invite admits two agents. Every branch of it is
//     tabulated below.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// igRiderKind is a rider kind that is neither auth.RecordKind nor the composite
// envelope. It stands in for invite.RecordKind so this file does not have to
// import internal/invite to test a package that deliberately does not import it
// either.
const igRiderKind = "invite"

// igRiderBody is a syntactically valid rider payload. Its CONTENT is opaque to
// internal/auth by design — the multiplexer hands it straight back to whichever
// applier owns igRiderKind — so a JSON object with a recognisable marker is
// exactly as much fidelity as this package can meaningfully assert on.
var igRiderBody = json.RawMessage(`{"v":1,"id":"inv-0000000000000001","state":"redeemed"}`)

// igEntry is the enrolment half of every composite built here.
func igEntry(t *testing.T, name string, n uint64) auth.RosterEntry {
	t.Helper()
	e := baseEntry(t, name, n)
	e.InviteID = "inv-0000000000000001"
	return e
}

// igRider is the standard rider.
func igRider() auth.InviteRider {
	return auth.InviteRider{Kind: igRiderKind, Body: igRiderBody}
}

// ---------------------------------------------------------------------------
// Encode / Decode
// ---------------------------------------------------------------------------

// TestInviteGateCompositeRoundTrip pins the on-disk shape and proves the two
// halves come back EXACTLY as their own packages produced them. "Exactly"
// matters: the whole reason each half is embedded verbatim as raw JSON is that
// the bytes handed to a participant at replay must be byte-for-byte the bytes
// it would have received had the record been written on its own.
func TestInviteGateCompositeRoundTrip(t *testing.T) {
	want := igEntry(t, "worker", 3)
	raw, err := auth.EncodeEnrolWithInvite(want, igRider())
	if err != nil {
		t.Fatalf("EncodeEnrolWithInvite: %v", err)
	}

	// The ENVELOPE's field names are the on-disk contract. A typed struct here
	// would tolerate a rename; a generic map will not.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("the composite record is not a JSON object: %v (%s)", err, raw)
	}
	var haveKeys []string
	for k := range envelope {
		haveKeys = append(haveKeys, k)
	}
	for _, k := range []string{"v", "enrolment", "rider_kind", "rider"} {
		if _, ok := envelope[k]; !ok {
			t.Fatalf("the composite record has no %q field; these names are written into an append-only log and later builds must still read them. got %v", k, haveKeys)
		}
	}
	if len(envelope) != 4 {
		t.Fatalf("the composite record carries %d fields (%v), want exactly the four documented ones", len(envelope), haveKeys)
	}
	if string(envelope["v"]) != "1" {
		t.Errorf(`"v" = %s, want 1 (auth.EnrolInviteRecordVersion)`, envelope["v"])
	}
	if string(envelope["rider_kind"]) != `"`+igRiderKind+`"` {
		t.Errorf(`"rider_kind" = %s, want %q`, envelope["rider_kind"], igRiderKind)
	}

	// The enrolment half is LITERALLY the bytes a plain enrolment record carries.
	plain, err := auth.Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(envelope["enrolment"], plain) {
		t.Errorf("the embedded enrolment half is not byte-identical to auth.Encode's output.\n  got  %s\n  want %s", envelope["enrolment"], plain)
	}
	if !bytes.Equal(envelope["rider"], igRiderBody) {
		t.Errorf("the embedded rider is not byte-identical to what the rider's own package produced.\n  got  %s\n  want %s", envelope["rider"], igRiderBody)
	}

	gotEntry, gotRider, err := auth.DecodeEnrolWithInvite(raw)
	if err != nil {
		t.Fatalf("DecodeEnrolWithInvite: %v", err)
	}
	if gotEntry.AgentID != want.AgentID || gotEntry.Name != want.Name || gotEntry.InviteID != want.InviteID {
		t.Errorf("the enrolment half round-tripped as %+v, want %+v", gotEntry, normaliseEntry(want))
	}
	if gotRider.Kind != igRiderKind {
		t.Errorf("the rider kind round-tripped as %q, want %q", gotRider.Kind, igRiderKind)
	}
	if !bytes.Equal(gotRider.Body, igRiderBody) {
		t.Errorf("the rider body round-tripped as %s, want %s", gotRider.Body, igRiderBody)
	}
}

// TestInviteGateEncodeRefusesAnUnstorableComposite pins the ENCODER's
// validation. It runs BEFORE the durable write, so a record that cannot be
// stored fails with NOTHING written — rather than being discovered as broken at
// replay, when the enrolment is durable, the invite is spent, and every
// remaining option is bad.
func TestInviteGateEncodeRefusesAnUnstorableComposite(t *testing.T) {
	good := igEntry(t, "worker", 3)

	tests := []struct {
		name  string
		entry auth.RosterEntry
		rider auth.InviteRider
		why   string
	}{
		{
			name:  "an unstorable enrolment half",
			entry: auth.RosterEntry{},
			rider: igRider(),
			why:   "the enrolment half goes through Encode, which validates",
		},
		{
			name:  "an empty rider kind",
			entry: good,
			rider: auth.InviteRider{Kind: "", Body: igRiderBody},
			why:   "nothing could apply it at replay",
		},
		{
			name:  "a rider kind that is the ENROLMENT kind",
			entry: good,
			rider: auth.InviteRider{Kind: auth.RecordKind, Body: igRiderBody},
			why:   "the multiplexer would hand the enrolment applier a body it cannot decode",
		},
		{
			name:  "a rider kind that is the COMPOSITE envelope",
			entry: good,
			rider: auth.InviteRider{Kind: auth.EnrolInviteRecordKind, Body: igRiderBody},
			why:   "the expansion would re-enter and apply the enrolment twice",
		},
		{
			name:  "an empty rider body",
			entry: good,
			rider: auth.InviteRider{Kind: igRiderKind, Body: nil},
			why:   "a rider that records nothing has no business sharing the enrolment's transaction",
		},
		{
			name:  "a whitespace-only rider body",
			entry: good,
			rider: auth.InviteRider{Kind: igRiderKind, Body: json.RawMessage("  \n\t ")},
			why:   "whitespace is not a record",
		},
		{
			name:  "a rider body that is not JSON",
			entry: good,
			rider: auth.InviteRider{Kind: igRiderKind, Body: json.RawMessage(`{"id":`)},
			why:   "this build would write a composite record no build can ever read back",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := auth.EncodeEnrolWithInvite(tc.entry, tc.rider)
			if err == nil {
				t.Fatalf("EncodeEnrolWithInvite accepted %s and produced %s; %s", tc.name, raw, tc.why)
			}
			if !errors.Is(err, auth.ErrInvalidRecord) {
				t.Fatalf("EncodeEnrolWithInvite(%s) = %v, want an ErrInvalidRecord", tc.name, err)
			}
			if raw != nil {
				t.Errorf("EncodeEnrolWithInvite returned %s alongside its error; a refused record must produce no bytes at all", raw)
			}
		})
	}
}

// TestInviteGateDecodeIsStrict pins the DECODER's strictness. Every case here is
// a record that could reach this function off disk — a newer build's schema, a
// smuggled second object, a corrupted field — and every one of them must be
// refused rather than half-interpreted.
func TestInviteGateDecodeIsStrict(t *testing.T) {
	good, err := auth.EncodeEnrolWithInvite(igEntry(t, "worker", 3), igRider())
	if err != nil {
		t.Fatalf("building the good composite: %v", err)
	}
	inner, err := auth.Encode(igEntry(t, "worker", 3))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	compose := func(v int, riderKind string, rider string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"v":%d,"enrolment":%s,"rider_kind":%q,"rider":%s}`, v, inner, riderKind, rider))
	}

	tests := []struct {
		name string
		raw  json.RawMessage
	}{
		{"an unknown field", json.RawMessage(fmt.Sprintf(`{"v":1,"enrolment":%s,"rider_kind":%q,"rider":%s,"extra":true}`, inner, igRiderKind, igRiderBody))},
		{"trailing data after the object", append(append(json.RawMessage(nil), good...), []byte(`{"v":1}`)...)},
		{"a future schema version", compose(2, igRiderKind, string(igRiderBody))},
		{"a past schema version", compose(0, igRiderKind, string(igRiderBody))},
		{"an empty rider kind", compose(1, "", string(igRiderBody))},
		{"a rider kind of " + auth.RecordKind, compose(1, auth.RecordKind, string(igRiderBody))},
		{"a rider kind of " + auth.EnrolInviteRecordKind, compose(1, auth.EnrolInviteRecordKind, string(igRiderBody))},
		{"an undecodable enrolment half", json.RawMessage(fmt.Sprintf(`{"v":1,"enrolment":{"v":1},"rider_kind":%q,"rider":%s}`, igRiderKind, igRiderBody))},
		{"not JSON at all", json.RawMessage(`not json`)},
		{"an empty body", json.RawMessage(nil)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, rider, err := auth.DecodeEnrolWithInvite(tc.raw)
			if err == nil {
				t.Fatalf("DecodeEnrolWithInvite accepted %s and returned (%+v, %+v); a record read off disk is UNTRUSTED INPUT", tc.name, e, rider)
			}
			if !errors.Is(err, auth.ErrInvalidRecord) {
				t.Fatalf("DecodeEnrolWithInvite(%s) = %v, want an ErrInvalidRecord", tc.name, err)
			}
			if e.AgentID != "" || rider.Kind != "" || rider.Body != nil {
				t.Errorf("DecodeEnrolWithInvite returned partial values alongside its error: (%+v, %+v)", e, rider)
			}
		})
	}

	// An EMPTY JSON OBJECT is a legitimate rider body: it is not this package's
	// business what a rider records, and the multiplexer hands the bytes
	// straight back to the applier that owns the kind.
	if _, _, err := auth.DecodeEnrolWithInvite(compose(1, igRiderKind, `{}`)); err != nil {
		t.Errorf("DecodeEnrolWithInvite refused an empty JSON object as a rider body: %v", err)
	}

	// SCOPE NOTE, so the absence above is deliberate rather than an oversight:
	// a rider body that is a JSON SCALAR (`null`, `""`, `0`) is accepted by both
	// the encoder and the decoder — "empty" is measured on the trimmed BYTES, and
	// those are four, two and one byte respectively. That is not asserted either
	// way here: it is caller misuse this package cannot reach (the only producer
	// is invite.Redemption.Consume, which always emits an object), and the
	// consequence is contained — the scalar is dispatched to the rider's own
	// applier, which rejects and logs it exactly as it would any other damaged
	// record. Recorded rather than pinned, so that tightening it later is not a
	// test change.
}

// ---------------------------------------------------------------------------
// MultiplexApplier
// ---------------------------------------------------------------------------

// igApplier records what it was dispatched, in order, into a SHARED sequence so
// the relative order of two participants is observable.
type igApplier struct {
	name string
	seq  *[]string
	got  []wal.Committed
	err  error
}

func (a *igApplier) Apply(c wal.Committed) error {
	*a.seq = append(*a.seq, a.name)
	a.got = append(a.got, c)
	return a.err
}

// TestInviteGateMultiplexConstruction pins every refusal in NewMultiplexApplier.
// Each one turns a silent, total, invisible failure into a loud one at startup.
func TestInviteGateMultiplexConstruction(t *testing.T) {
	var seq []string
	ok := &igApplier{name: "enrol", seq: &seq}

	tests := []struct {
		name   string
		byKind map[string]wal.Applier
		want   string
	}{
		{"a nil map", nil, "at least one per-kind applier is required"},
		{"an empty map", map[string]wal.Applier{}, "at least one per-kind applier is required"},
		{"the empty kind", map[string]wal.Applier{"": ok}, "empty kind"},
		{"a nil applier", map[string]wal.Applier{auth.RecordKind: nil}, "is nil"},
		{"the composite kind", map[string]wal.Applier{auth.EnrolInviteRecordKind: ok}, "may not be registered"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := auth.NewMultiplexApplier(nil, tc.byKind)
			if err == nil {
				t.Fatalf("NewMultiplexApplier accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewMultiplexApplier(%s) = %v, want an error mentioning %q", tc.name, err, tc.want)
			}
			if m != nil {
				t.Errorf("NewMultiplexApplier returned a multiplexer alongside its error")
			}
		})
	}

	// A nil applier inside an otherwise-good map is caught too: it is the case
	// that would otherwise silently drop every record of that kind.
	if _, err := auth.NewMultiplexApplier(nil, map[string]wal.Applier{auth.RecordKind: ok, igRiderKind: nil}); err == nil {
		t.Errorf("NewMultiplexApplier accepted a map with one good applier and one nil one")
	}
}

// TestInviteGateMultiplexTableIsCopied proves the dispatch table cannot be
// mutated after the log has been opened with it. A mid-replay change of applier
// would silently split one record kind's history across two serving copies.
func TestInviteGateMultiplexTableIsCopied(t *testing.T) {
	var seq []string
	enrol := &igApplier{name: "enrol", seq: &seq}
	byKind := map[string]wal.Applier{auth.RecordKind: enrol}
	m, err := auth.NewMultiplexApplier(nil, byKind)
	if err != nil {
		t.Fatalf("NewMultiplexApplier: %v", err)
	}

	sneak := &igApplier{name: "sneak", seq: &seq}
	byKind[auth.RecordKind] = sneak

	body, err := auth.Encode(igEntry(t, "worker", 1))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := m.Apply(wal.Committed{PrepareIndex: 1, CommitIndex: 2, Entry: wal.Entry{Kind: auth.RecordKind, Body: body}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(sneak.got) != 0 {
		t.Fatalf("the applier substituted into the caller's map AFTER construction received %d records; the table must be copied", len(sneak.got))
	}
	if len(enrol.got) != 1 {
		t.Fatalf("the registered applier received %d records, want 1", len(enrol.got))
	}
}

// TestInviteGateMultiplexExpandsAComposite is the core dispatch claim: ONE
// composite entry becomes TWO records, enrolment half FIRST, each carrying the
// bytes its own package produced and the wal indices of the entry they came
// from.
func TestInviteGateMultiplexExpandsAComposite(t *testing.T) {
	var seq []string
	enrol := &igApplier{name: "enrol", seq: &seq}
	rider := &igApplier{name: "rider", seq: &seq}
	m, err := auth.NewMultiplexApplier(nil, map[string]wal.Applier{
		auth.RecordKind: enrol,
		igRiderKind:     rider,
	})
	if err != nil {
		t.Fatalf("NewMultiplexApplier: %v", err)
	}

	entry := igEntry(t, "worker", 9)
	body, err := auth.EncodeEnrolWithInvite(entry, igRider())
	if err != nil {
		t.Fatalf("EncodeEnrolWithInvite: %v", err)
	}
	c := wal.Committed{PrepareIndex: 41, CommitIndex: 42, Entry: wal.Entry{Kind: auth.EnrolInviteRecordKind, Body: body}}
	if err := m.Apply(c); err != nil {
		t.Fatalf("Apply of a composite entry = %v, want nil", err)
	}

	if want := []string{"enrol", "rider"}; strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("the composite was dispatched as %v, want %v: the enrolment half goes FIRST, so a rider applier that reads the roster sees the agent the invite was spent on", seq, want)
	}
	if len(enrol.got) != 1 || len(rider.got) != 1 {
		t.Fatalf("the composite produced %d enrolment records and %d rider records, want 1 of each", len(enrol.got), len(rider.got))
	}

	gotEnrol := enrol.got[0]
	if gotEnrol.Entry.Kind != auth.RecordKind {
		t.Errorf("the enrolment half was dispatched with kind %q, want %q", gotEnrol.Entry.Kind, auth.RecordKind)
	}
	wantPlain, err := auth.Encode(entry)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(gotEnrol.Entry.Body, wantPlain) {
		t.Errorf("the enrolment applier was handed %s, want the bytes a PLAIN enrolment record carries: %s", gotEnrol.Entry.Body, wantPlain)
	}

	gotRider := rider.got[0]
	if gotRider.Entry.Kind != igRiderKind {
		t.Errorf("the rider was dispatched with kind %q, want %q", gotRider.Entry.Kind, igRiderKind)
	}
	if !bytes.Equal(gotRider.Entry.Body, igRiderBody) {
		t.Errorf("the rider applier was handed %s, want the rider's own bytes verbatim: %s", gotRider.Entry.Body, igRiderBody)
	}

	// The wal indices travel with BOTH halves, or a participant that logs a
	// discard names a record nobody can find.
	for _, got := range []wal.Committed{gotEnrol, gotRider} {
		if got.PrepareIndex != 41 || got.CommitIndex != 42 {
			t.Errorf("a half was dispatched with indices prepare=%d commit=%d, want 41/42", got.PrepareIndex, got.CommitIndex)
		}
	}
}

// TestInviteGateMultiplexAnUndecodableCompositeReturnsNilAndSaysSo is the error
// policy, and it is the assertion that matters most in this file after the
// crash points.
//
// NIL, NEVER AN ERROR. A non-nil error here poisons the log on a LIVE write
// (wal.ErrDiverged) and makes wal.Open FAIL on recovery — a bus that refuses to
// boot over one damaged record. Invariant 6 settled that trade: recovery ALWAYS
// reaches a running server, damaged records are discarded, and the ABSOLUTE half
// of the bargain is that every discard is LOGGED, specifically. So both halves
// are asserted: the nil, and the ERROR line naming the record.
func TestInviteGateMultiplexAnUndecodableCompositeReturnsNilAndSaysSo(t *testing.T) {
	var seq []string
	enrol := &igApplier{name: "enrol", seq: &seq}
	rider := &igApplier{name: "rider", seq: &seq}

	var logBuf bytes.Buffer
	m, err := auth.NewMultiplexApplier(logging.New(&logBuf, logging.LevelDebug), map[string]wal.Applier{
		auth.RecordKind: enrol,
		igRiderKind:     rider,
	})
	if err != nil {
		t.Fatalf("NewMultiplexApplier: %v", err)
	}

	c := wal.Committed{
		PrepareIndex: 17,
		CommitIndex:  18,
		Entry:        wal.Entry{Kind: auth.EnrolInviteRecordKind, Body: json.RawMessage(`{"v":1,"enrolment":`)},
	}
	if err := m.Apply(c); err != nil {
		t.Fatalf(`Apply of an UNDECODABLE composite returned %v, want nil.

An error here is not a stricter policy, it is an OUTAGE: on a live commit it
poisons the log with wal.ErrDiverged, and on recovery it makes wal.Open fail so
the bus never starts. Invariant 6 requires that recovery ALWAYS reaches a
running server.`, err)
	}
	if len(enrol.got) != 0 || len(rider.got) != 0 {
		t.Fatalf("an undecodable composite dispatched %d enrolment and %d rider records; BOTH halves are lost, neither is guessed at", len(enrol.got), len(rider.got))
	}

	line := discardLine(t, logBuf.String(), "DISCARDING a composite")
	if !strings.Contains(line, "level=error") {
		t.Errorf("the discard was logged at the wrong level; a lost enrolment AND an unspent invite is an ERROR.\n  line: %s", line)
	}
	assertNamesTheRecord(t, line)
	// The line must be actionable, not merely present: the invite half is
	// FAIL-OPEN here, and an operator who does not know that will assume the
	// discard was safe.
	for _, want := range []string{"REDEEMABLE", "re-enrol"} {
		if !strings.Contains(line, want) {
			t.Errorf("the discard line does not mention %q; an operator cannot act on it.\n  line: %s", want, line)
		}
	}
}

// TestInviteGateMultiplexAnUnregisteredRiderKindIsLoggedAndSkipped covers the
// case where the composite decodes perfectly but names a rider kind nothing is
// registered for — a mis-wiring, not damage.
//
// It must NOT fail (same reasoning as above), the ENROLMENT half must still be
// applied, and the loss must be reported at ERROR: the invite is not marked
// spent in the serving copy, so it may be redeemable AGAIN until a restart with
// the applier wired.
func TestInviteGateMultiplexAnUnregisteredRiderKindIsLoggedAndSkipped(t *testing.T) {
	var seq []string
	enrol := &igApplier{name: "enrol", seq: &seq}

	var logBuf bytes.Buffer
	m, err := auth.NewMultiplexApplier(logging.New(&logBuf, logging.LevelDebug), map[string]wal.Applier{
		auth.RecordKind: enrol,
	})
	if err != nil {
		t.Fatalf("NewMultiplexApplier: %v", err)
	}

	entry := igEntry(t, "worker", 4)
	body, err := auth.EncodeEnrolWithInvite(entry, auth.InviteRider{Kind: "a-kind-nobody-owns", Body: igRiderBody})
	if err != nil {
		t.Fatalf("EncodeEnrolWithInvite: %v", err)
	}
	if err := m.Apply(wal.Committed{PrepareIndex: 5, CommitIndex: 6, Entry: wal.Entry{Kind: auth.EnrolInviteRecordKind, Body: body}}); err != nil {
		t.Fatalf("Apply with an unregistered rider kind = %v, want nil", err)
	}
	if len(enrol.got) != 1 {
		t.Fatalf("the ENROLMENT half was not applied (%d records); the missing applier is the RIDER's, and losing the agent too would double the damage", len(enrol.got))
	}

	line := discardLine(t, logBuf.String(), "names a rider kind with NO registered applier")
	if !strings.Contains(line, "level=error") {
		t.Errorf("the unapplied rider was logged at the wrong level.\n  line: %s", line)
	}
	assertNamesTheRecord(t, line)
	if !strings.Contains(line, "rider_kind=a-kind-nobody-owns") {
		t.Errorf("the line does not name the rider kind nothing was registered for, which is the one fact an operator needs to fix the wiring.\n  line: %s", line)
	}
}

// TestInviteGateMultiplexIgnoresAKindItDoesNotOwn pins the SILENT skip, and it
// is deliberately NOT the same policy as internal/wal's MultiApplier.
//
// This log also carries store "message" records and hub "seqfloor" records,
// which the hub applies through its OWN read-only replay pass. Those records are
// a NEIGHBOUR's, not damage — treating them as damage would fill the log with
// false alarms on every single start.
func TestInviteGateMultiplexIgnoresAKindItDoesNotOwn(t *testing.T) {
	var seq []string
	enrol := &igApplier{name: "enrol", seq: &seq}

	var logBuf bytes.Buffer
	m, err := auth.NewMultiplexApplier(logging.New(&logBuf, logging.LevelDebug), map[string]wal.Applier{
		auth.RecordKind: enrol,
	})
	if err != nil {
		t.Fatalf("NewMultiplexApplier: %v", err)
	}

	for _, kind := range []string{"message", "seqfloor", "peer", ""} {
		if err := m.Apply(wal.Committed{PrepareIndex: 1, CommitIndex: 2, Entry: wal.Entry{Kind: kind, Body: json.RawMessage(`{"anything":true}`)}}); err != nil {
			t.Fatalf("Apply of a %q record = %v, want nil", kind, err)
		}
	}
	if len(enrol.got) != 0 {
		t.Fatalf("a record of another kind reached the enrolment applier")
	}
	if logBuf.Len() != 0 {
		t.Fatalf(`a record of a neighbouring kind produced log output:
%s
Those records belong to another participant's replay pass. Reporting them would
put a discard line in the log for every message this bus has ever sent.`, logBuf.String())
	}
}

// TestInviteGateMultiplexReturnsAParticipantsError pins the one case that IS
// propagated: a participant that decides a failure is fatal enough to return has
// made that judgement deliberately, and the multiplexer is not the place to
// overrule it.
func TestInviteGateMultiplexReturnsAParticipantsError(t *testing.T) {
	boom := errors.New("the participant said no")

	t.Run("from the enrolment half of a composite", func(t *testing.T) {
		var seq []string
		enrol := &igApplier{name: "enrol", seq: &seq, err: boom}
		rider := &igApplier{name: "rider", seq: &seq}
		m, err := auth.NewMultiplexApplier(nil, map[string]wal.Applier{auth.RecordKind: enrol, igRiderKind: rider})
		if err != nil {
			t.Fatalf("NewMultiplexApplier: %v", err)
		}
		body, err := auth.EncodeEnrolWithInvite(igEntry(t, "worker", 2), igRider())
		if err != nil {
			t.Fatalf("EncodeEnrolWithInvite: %v", err)
		}
		if err := m.Apply(wal.Committed{Entry: wal.Entry{Kind: auth.EnrolInviteRecordKind, Body: body}}); !errors.Is(err, boom) {
			t.Fatalf("Apply = %v, want the participant's own error", err)
		}
		if len(rider.got) != 0 {
			t.Errorf("the rider was dispatched after the enrolment half failed; expansion stops at the first refusal")
		}
	})

	t.Run("from the rider half of a composite", func(t *testing.T) {
		var seq []string
		enrol := &igApplier{name: "enrol", seq: &seq}
		rider := &igApplier{name: "rider", seq: &seq, err: boom}
		m, err := auth.NewMultiplexApplier(nil, map[string]wal.Applier{auth.RecordKind: enrol, igRiderKind: rider})
		if err != nil {
			t.Fatalf("NewMultiplexApplier: %v", err)
		}
		body, err := auth.EncodeEnrolWithInvite(igEntry(t, "worker", 2), igRider())
		if err != nil {
			t.Fatalf("EncodeEnrolWithInvite: %v", err)
		}
		if err := m.Apply(wal.Committed{Entry: wal.Entry{Kind: auth.EnrolInviteRecordKind, Body: body}}); !errors.Is(err, boom) {
			t.Fatalf("Apply = %v, want the participant's own error", err)
		}
	})

	t.Run("from a plain non-composite record", func(t *testing.T) {
		var seq []string
		enrol := &igApplier{name: "enrol", seq: &seq, err: boom}
		m, err := auth.NewMultiplexApplier(nil, map[string]wal.Applier{auth.RecordKind: enrol})
		if err != nil {
			t.Fatalf("NewMultiplexApplier: %v", err)
		}
		body, err := auth.Encode(igEntry(t, "worker", 2))
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if err := m.Apply(wal.Committed{Entry: wal.Entry{Kind: auth.RecordKind, Body: body}}); !errors.Is(err, boom) {
			t.Fatalf("Apply = %v, want the participant's own error", err)
		}
	})
}

// ---------------------------------------------------------------------------
// PutWithInvite: the `durable` flag
// ---------------------------------------------------------------------------

// igWriter is an auth.DurableWriter under full test control. When applier is
// non-nil a successful Write also runs it, which is what a real *wal.Log does
// after the commit fsync — and without it PutWithInvite's mis-wiring check would
// fire on every "success".
type igWriter struct {
	err     error
	applier wal.Applier
	entries []wal.Entry
}

func (w *igWriter) Write(e wal.Entry) (wal.Committed, error) {
	w.entries = append(w.entries, e)
	if w.err != nil {
		return wal.Committed{}, w.err
	}
	c := wal.Committed{PrepareIndex: uint64(2*len(w.entries) - 1), CommitIndex: uint64(2 * len(w.entries)), Entry: e}
	if w.applier != nil {
		if err := w.applier.Apply(c); err != nil {
			return wal.Committed{}, err
		}
	}
	return c, nil
}

// igAttachedRoster builds a WALRoster attached to w, with the multiplexer as the
// thing w applies through — i.e. the shipped wiring, minus the disk.
func igAttachedRoster(t *testing.T, w *igWriter) *auth.WALRoster {
	t.Helper()
	r := auth.NewWALRoster(nil)
	m, err := auth.NewMultiplexApplier(nil, map[string]wal.Applier{
		auth.RecordKind: r,
		igRiderKind:     &igApplier{name: "rider", seq: new([]string)},
	})
	if err != nil {
		t.Fatalf("NewMultiplexApplier: %v", err)
	}
	w.applier = m
	if err := r.Attach(w); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return r
}

// TestInviteGatePutWithInviteDurableFlag is the table the whole invite gate's
// safety rests on.
//
// The caller (auth.Service.Enrol) decides an invite's fate by this flag: on
// durable == true it must NOT abort the reservation, because the consumption
// record is ALREADY on stable storage and releasing it would admit a SECOND
// redemption of a spent invite — one invite, two agents. On durable == false it
// MUST abort, or a failed enrolment strands the invite until a restart.
//
// So a wrong answer in EITHER direction is a real defect, and every branch is
// enumerated.
func TestInviteGatePutWithInviteDurableFlag(t *testing.T) {
	t.Run("a successful write is durable and applied", func(t *testing.T) {
		w := &igWriter{}
		r := igAttachedRoster(t, w)
		e := igEntry(t, "worker", 1)

		durable, err := r.PutWithInvite(e, igRider())
		if err != nil {
			t.Fatalf("PutWithInvite = %v, want nil", err)
		}
		if !durable {
			t.Fatalf("a SUCCESSFUL PutWithInvite reported durable=false; the caller would abort a reservation whose consumption record is on disk")
		}
		if len(w.entries) != 1 {
			t.Fatalf("PutWithInvite wrote %d entries, want exactly 1: the enrolment and the consumption are ONE transaction", len(w.entries))
		}
		if w.entries[0].Kind != auth.EnrolInviteRecordKind {
			t.Fatalf("PutWithInvite wrote an entry of kind %q, want %q", w.entries[0].Kind, auth.EnrolInviteRecordKind)
		}
		if _, ok := r.Get(e.AgentID); !ok {
			t.Fatalf("the agent is not in the serving roster after a successful PutWithInvite")
		}
	})

	t.Run("an UNATTACHED roster is not durable", func(t *testing.T) {
		r := auth.NewWALRoster(nil)
		durable, err := r.PutWithInvite(igEntry(t, "worker", 1), igRider())
		if !errors.Is(err, auth.ErrNotAttached) {
			t.Fatalf("PutWithInvite on an unattached roster = %v, want ErrNotAttached", err)
		}
		if durable {
			t.Fatalf("PutWithInvite reported durable=true with NO log attached; the caller would refuse to release a reservation nothing was ever written for, stranding the invite until restart")
		}
	})

	t.Run("a DUPLICATE agent id is not durable and burns no fsync", func(t *testing.T) {
		w := &igWriter{}
		r := igAttachedRoster(t, w)
		e := igEntry(t, "worker", 1)
		if _, err := r.PutWithInvite(e, igRider()); err != nil {
			t.Fatalf("first PutWithInvite: %v", err)
		}
		durable, err := r.PutWithInvite(e, igRider())
		if !errors.Is(err, auth.ErrDuplicateAgentID) {
			t.Fatalf("a duplicate PutWithInvite = %v, want ErrDuplicateAgentID", err)
		}
		if durable {
			t.Fatalf("a REFUSED duplicate reported durable=true; nothing was written, so the caller must release the reservation")
		}
		if len(w.entries) != 1 {
			t.Fatalf("the duplicate reached the log (%d entries, want 1)", len(w.entries))
		}
	})

	t.Run("an unstorable entry is not durable and writes nothing", func(t *testing.T) {
		w := &igWriter{}
		r := igAttachedRoster(t, w)
		durable, err := r.PutWithInvite(auth.RosterEntry{}, igRider())
		if err == nil {
			t.Fatalf("PutWithInvite accepted an empty roster entry")
		}
		if durable {
			t.Fatalf("a refused entry reported durable=true")
		}
		if len(w.entries) != 0 {
			t.Fatalf("a refused entry reached the log")
		}
	})

	t.Run("an unstorable RIDER is not durable and writes nothing", func(t *testing.T) {
		w := &igWriter{}
		r := igAttachedRoster(t, w)
		durable, err := r.PutWithInvite(igEntry(t, "worker", 1), auth.InviteRider{Kind: igRiderKind, Body: json.RawMessage(`{`)})
		if !errors.Is(err, auth.ErrInvalidRecord) {
			t.Fatalf("PutWithInvite with a malformed rider = %v, want ErrInvalidRecord", err)
		}
		if durable {
			t.Fatalf("a refused rider reported durable=true")
		}
		if len(w.entries) != 0 {
			t.Fatalf("a refused rider reached the log")
		}
	})

	t.Run("wal.ErrDiverged IS durable", func(t *testing.T) {
		// THE LOAD-BEARING CASE. wal.Txn.Commit returns ErrDiverged AFTER the
		// commit record has been appended and FSYNCED: the entry — including the
		// invite consumption record inside it — is on stable storage, and only a
		// neighbouring applier failed. A caller that aborted here would leave
		// memory saying OPEN while disk says REDEEMED, and the next attempt would
		// be a SECOND redemption of a spent invite.
		//
		// It is WRAPPED, not returned bare, because that is how a real log
		// returns it and because a naive `err == wal.ErrDiverged` would pass a
		// bare-sentinel test and fail here.
		w := &igWriter{err: fmt.Errorf("wal: applying committed entry 7: %w", wal.ErrDiverged)}
		r := igAttachedRoster(t, w)

		durable, err := r.PutWithInvite(igEntry(t, "worker", 1), igRider())
		if err == nil {
			t.Fatalf("PutWithInvite returned nil for a diverged write")
		}
		if !errors.Is(err, wal.ErrDiverged) {
			t.Fatalf("PutWithInvite = %v; the underlying sentinel must survive wrapping or a caller cannot classify it", err)
		}
		if !durable {
			t.Fatalf(`PutWithInvite reported durable=FALSE for a wal.ErrDiverged write.

ErrDiverged is returned AFTER the commit record is appended and fsynced, so the
composite entry -- INCLUDING the invite consumption record -- IS on stable
storage. Reporting it as not durable makes the caller release the reservation,
memory says OPEN while disk says REDEEMED, and ONE INVITE ADMITS TWO AGENTS.`)
		}
	})

	t.Run("any other write failure is NOT durable", func(t *testing.T) {
		w := &igWriter{err: errors.New("disk on fire")}
		r := igAttachedRoster(t, w)
		durable, err := r.PutWithInvite(igEntry(t, "worker", 1), igRider())
		if err == nil {
			t.Fatalf("PutWithInvite returned nil for a failed write")
		}
		if durable {
			t.Fatalf("an ordinary write failure reported durable=true; the invite would stay locked until a restart for no reason")
		}
		if _, ok := r.Get(mustAgentID(t, "worker", 1)); ok {
			t.Fatalf("a failed write left the agent in the serving roster; memory must still match disk")
		}
	})

	t.Run("durable but ABSENT from the serving roster is an error AND durable", func(t *testing.T) {
		// The mis-wiring detector: the log was opened with an applier that is not
		// this roster (here, none at all), so the record is on disk and the
		// serving copy is empty. It must FAIL — acknowledging an enrolment the
		// serving copy does not have would tell an agent an id it can never
		// authenticate with — and it must report durable=TRUE, because the record
		// really is on disk and the invite really is spent.
		w := &igWriter{}
		r := auth.NewWALRoster(nil)
		if err := r.Attach(w); err != nil {
			t.Fatalf("Attach: %v", err)
		}
		durable, err := r.PutWithInvite(igEntry(t, "worker", 1), igRider())
		if err == nil {
			t.Fatalf("PutWithInvite succeeded although the entry never reached the serving roster")
		}
		if !strings.Contains(err.Error(), "ABSENT from the serving roster") {
			t.Errorf("PutWithInvite = %v, want the mis-wiring error", err)
		}
		if !durable {
			t.Fatalf("the mis-wiring case reported durable=false; the record IS on disk, so the caller must not un-spend the invite over it")
		}
	})

	t.Run("Put and PutWithInvite share one insertion path", func(t *testing.T) {
		// Plain Put must still write a PLAIN record. If it ever started writing
		// composites, every pre-INVITE-GATE data directory would stop replaying.
		w := &igWriter{}
		r := igAttachedRoster(t, w)
		if err := r.Put(igEntry(t, "worker", 1)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if w.entries[0].Kind != auth.RecordKind {
			t.Fatalf("Put wrote an entry of kind %q, want %q", w.entries[0].Kind, auth.RecordKind)
		}
	})
}

// ---------------------------------------------------------------------------
// The suffix derivation must SEE a composite record
// ---------------------------------------------------------------------------

// TestInviteGateSuffixFloorsFoldTheCompositeHalf is an invariant-1 assertion
// wearing a derivation's clothes.
//
// An invited enrolment burned a suffix exactly as a plain one did, but its agent
// id sits INSIDE the composite envelope rather than at the top level of the
// prepare body. A fold that looked only at auth.RecordKind would derive a LOWER
// floor on any bus that has ever gated an enrolment — and a floor too low
// re-mints an agent id that is already on disk, handing a NEW agent holding a
// DIFFERENT keypair the identity an earlier one used.
//
// This is the fast unit half; TestInviteGateCrashAfterPrepare proves the same
// thing over a real SIGKILLed process.
func TestInviteGateSuffixFloorsFoldTheCompositeHalf(t *testing.T) {
	dir := t.TempDir()
	l := openPlainLog(t, dir)

	plain, err := auth.Encode(baseEntry(t, "worker", 2))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := l.Write(wal.Entry{Kind: auth.RecordKind, Body: plain}); err != nil {
		t.Fatalf("writing the plain enrolment: %v", err)
	}
	composite, err := auth.EncodeEnrolWithInvite(igEntry(t, "worker", 11), igRider())
	if err != nil {
		t.Fatalf("EncodeEnrolWithInvite: %v", err)
	}
	if _, err := l.Write(wal.Entry{Kind: auth.EnrolInviteRecordKind, Body: composite}); err != nil {
		t.Fatalf("writing the composite enrolment: %v", err)
	}
	// A composite for ANOTHER bus must be skipped, exactly as a plain foreign
	// record is: a foreign id burned no local suffix.
	foreign := igEntry(t, "worker", 99)
	foreign.AgentID = "some-other-bus.worker-99"
	foreignBody, err := auth.EncodeEnrolWithInvite(foreign, igRider())
	if err != nil {
		t.Fatalf("EncodeEnrolWithInvite (foreign): %v", err)
	}
	if _, err := l.Write(wal.Entry{Kind: auth.EnrolInviteRecordKind, Body: foreignBody}); err != nil {
		t.Fatalf("writing the foreign composite: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	floors, err := auth.EnrolmentSuffixesInWAL(walPath(dir), testBusID)
	if err != nil {
		t.Fatalf("EnrolmentSuffixesInWAL: %v", err)
	}
	if got := floors["worker"]; got != 11 {
		t.Fatalf(`the floor for "worker" is %d, want 11.

The suffix was burned by an INVITED enrolment, whose agent id lives inside the
composite envelope. A derivation that folds only %q records reports 2 here, and
the next three "worker" enrolments are minted onto ids this bus has already
written down (invariant 1). This is one half of the union
cmd/agent-bus/suffixfloors.go derives for -backfill-suffix-floors, so getting it
wrong re-mints ids on the RECOVERY path too.`, got, auth.RecordKind)
	}
	if len(floors) != 1 {
		t.Errorf("the derivation reports %d names (%v), want only the local one: a foreign bus's composite burned no local suffix", len(floors), floors)
	}
}

// TestInviteGateSuffixFloorsFailTotallyOnABrokenComposite pins the deliberate
// exception to floors.go's leniency: the composite envelope is unwrapped with
// the STRICT decoder, and a composite this build cannot read makes the WHOLE
// derivation fail rather than silently contributing nothing.
//
// Fail-loud is the safe direction here. A silently unread composite LOWERS the
// floor, and a floor too low is the identity-reuse failure the derivation exists
// to prevent — so "I could not read this record" must never look like "this
// record burned no suffix".
func TestInviteGateSuffixFloorsFailTotallyOnABrokenComposite(t *testing.T) {
	dir := t.TempDir()
	l := openPlainLog(t, dir)

	plain, err := auth.Encode(baseEntry(t, "worker", 2))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := l.Write(wal.Entry{Kind: auth.RecordKind, Body: plain}); err != nil {
		t.Fatalf("writing the plain enrolment: %v", err)
	}
	if _, err := l.Write(wal.Entry{Kind: auth.EnrolInviteRecordKind, Body: json.RawMessage(`{"v":99,"enrolment":{},"rider_kind":"invite","rider":{}}`)}); err != nil {
		t.Fatalf("writing the broken composite: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	floors, err := auth.EnrolmentSuffixesInWAL(walPath(dir), testBusID)
	if err == nil {
		t.Fatalf("EnrolmentSuffixesInWAL returned %v and a nil error over a log holding an undecodable composite; a caller cannot tell an incomplete scan from a complete one, and an incomplete one seals a floor that is too low", floors)
	}
	if len(floors) != 0 {
		t.Fatalf("EnrolmentSuffixesInWAL returned a %d-entry map alongside its error, want none; failure is TOTAL", len(floors))
	}
	if !strings.Contains(err.Error(), "composite") {
		t.Errorf("the error does not say which record shape failed: %v", err)
	}
}
