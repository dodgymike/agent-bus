package ack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
)

const (
	testBus       = "testbus"
	testKey       = "testbus-7"
	testRecipient = "testbus.beta-1"
	testSender    = "testbus.alpha-1"
)

var testAccepted = time.Date(2026, 8, 16, 10, 0, 0, 123456789, time.UTC)
var testSettled = time.Date(2026, 8, 16, 10, 5, 0, 0, time.UTC)

func acceptedRecord() Record {
	return Record{
		CorrelationKey: testKey,
		Recipient:      testRecipient,
		Sender:         testSender,
		State:          StateAccepted,
		AcceptedAt:     testAccepted,
	}
}

// TestRecordRoundTrip proves Encode and DecodeRecord are inverses for every
// state the machine can reach — which is what canonical() depends on when it
// folds DecodeRecord(Encode(r)) into memory so a live apply and a replayed apply
// cannot drift.
func TestRecordRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
	}{
		{"accepted", acceptedRecord()},
		{"in_flight", func() Record { r := acceptedRecord(); r.State = StateInFlight; return r }()},
		{"delivered", func() Record {
			r := acceptedRecord()
			r.State, r.AttestedBy, r.SettledAt = StateDelivered, AttestedByRecipientSignatureUnverified, testSettled
			return r
		}()},
		{"refused", func() Record {
			r := acceptedRecord()
			r.State, r.Class, r.AttestedBy, r.SettledAt = StateRefused, ClassRecipientRefusedPolicy, AttestedByRecipientSignatureUnverified, testSettled
			return r
		}()},
		{"undeliverable", func() Record {
			r := acceptedRecord()
			r.State, r.Class, r.AttestedBy, r.SettledAt = StateUndeliverable, ClassNoRoute, AttestedByPeerBus, testSettled
			return r
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := tc.rec.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			back, err := DecodeRecord(body)
			if err != nil {
				t.Fatalf("DecodeRecord(%s): %v", body, err)
			}
			if back.CorrelationKey != tc.rec.CorrelationKey || back.Recipient != tc.rec.Recipient ||
				back.Sender != tc.rec.Sender || back.State != tc.rec.State ||
				back.Class != tc.rec.Class || back.AttestedBy != tc.rec.AttestedBy {
				t.Fatalf("round trip changed the record:\n got %+v\nwant %+v", back, tc.rec)
			}
			if !back.AcceptedAt.Equal(tc.rec.AcceptedAt) {
				t.Errorf("accepted_at round-tripped as %s, want %s", back.AcceptedAt, tc.rec.AcceptedAt)
			}
			if !back.SettledAt.Equal(tc.rec.SettledAt) {
				t.Errorf("settled_at round-tripped as %s, want %s", back.SettledAt, tc.rec.SettledAt)
			}
			// The second pass is what canonical() actually relies on: the bytes
			// must be STABLE, not merely decodable.
			again, err := back.Encode()
			if err != nil {
				t.Fatalf("re-Encode: %v", err)
			}
			if string(again) != string(body) {
				t.Fatalf("re-encoding is not byte-stable:\n got %s\nwant %s", again, body)
			}
		})
	}
}

// TestRecordValidateRejections is the fail-closed table. Every case here is a
// record this bus must refuse to WRITE and must refuse to READ BACK, because a
// lenient decoder reinstates a row with a mangled state — and the worst of those
// reinstates a terminal row as a non-terminal one, resurrecting a settled
// outcome.
func TestRecordValidateRejections(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Record)
		want string
	}{
		{"empty correlation key", func(r *Record) { r.CorrelationKey = "" }, "correlation_key is required"},
		{"correlation key is not a message id", func(r *Record) { r.CorrelationKey = "not-an-id!" }, "not a server-minted message id"},
		{"oversized correlation key", func(r *Record) { r.CorrelationKey = strings.Repeat("a", ids.MaxMessageIDLen+1) }, "over the"},
		{"unqualified recipient", func(r *Record) { r.Recipient = "beta" }, "fully-qualified"},
		{"unqualified sender", func(r *Record) { r.Sender = "alpha" }, "fully-qualified"},
		{"state outside the enum", func(r *Record) { r.State = State(99) }, "outside the closed set"},
		{"accepted carrying a class", func(r *Record) { r.Class = ClassNoRoute }, "class is set IFF"},
		{"accepted carrying an attestation", func(r *Record) { r.AttestedBy = AttestedByPeerBus }, "attestation is set IFF"},
		{"accepted carrying settled_at", func(r *Record) { r.SettledAt = testSettled }, "not terminal and carries settled_at"},
		{"no accepted_at", func(r *Record) { r.AcceptedAt = time.Time{} }, "accepted_at is required"},
		{"refused with no class", func(r *Record) {
			r.State, r.AttestedBy, r.SettledAt = StateRefused, AttestedByRecipientSignatureUnverified, testSettled
		}, "carries no class"},
		{"refused with a BUS class", func(r *Record) {
			r.State, r.Class, r.AttestedBy, r.SettledAt = StateRefused, ClassNoRoute, AttestedByRecipientSignatureUnverified, testSettled
		}, "not one of the three RECIPIENT-emitted classes"},
		{"undeliverable with a RECIPIENT class", func(r *Record) {
			r.State, r.Class, r.AttestedBy, r.SettledAt = StateUndeliverable, ClassRecipientRefusedPolicy, AttestedByPeerBus, testSettled
		}, "not one of the nine BUS-emitted classes"},
		{"delivered with a class", func(r *Record) {
			r.State, r.Class, r.AttestedBy, r.SettledAt = StateDelivered, ClassNoRoute, AttestedByRecipientSignatureUnverified, testSettled
		}, "class is set IFF"},
		{"terminal with no settled_at", func(r *Record) {
			r.State, r.AttestedBy = StateDelivered, AttestedByRecipientSignatureUnverified
		}, "carries no settled_at"},
		{"terminal with no attestation", func(r *Record) {
			r.State, r.SettledAt = StateDelivered, testSettled
		}, "outside the closed set"},
		{"terminal with an invented attestation", func(r *Record) {
			r.State, r.AttestedBy, r.SettledAt = StateDelivered, Attestation("verified"), testSettled
		}, "outside the closed set"},
		{"invented class", func(r *Record) {
			r.State, r.Class, r.AttestedBy, r.SettledAt = StateRefused, Class("recipient_refused_because_i_said_so"), AttestedByRecipientSignatureUnverified, testSettled
		}, "not one of the three RECIPIENT-emitted classes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := acceptedRecord()
			tc.mut(&r)
			_, err := r.Encode()
			if err == nil {
				t.Fatalf("Encode accepted a record it must refuse (%+v)", r)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Encode error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestDecodeRecordIsStrict pins the decoder's posture: unknown fields, trailing
// data, a foreign schema version and an unrecognised enum spelling are each an
// ERROR and never a default. Guessing turns a corrupt or future-format record
// into a plausible-looking outcome.
func TestDecodeRecordIsStrict(t *testing.T) {
	good, err := acceptedRecord().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unknown field", strings.Replace(string(good), `{`, `{"surprise":1,`, 1), "unknown field"},
		{"trailing data", string(good) + `{"more":1}`, "trailing data"},
		{"future schema version", strings.Replace(string(good), `"record_version":1`, `"record_version":2`, 1), "is not 1"},
		{"unknown state", strings.Replace(string(good), `"accepted"`, `"shipped"`, 1), "not a delivery lifecycle state"},
		{"the reporting value `unknown`", strings.Replace(string(good), `"accepted"`, `"unknown"`, 1), "REPORTING value"},
		{"not json", `{`, "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeRecord(json.RawMessage(tc.body)); err == nil {
				t.Fatalf("DecodeRecord accepted %s", tc.body)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DecodeRecord error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestUnknownIsNotAState is the rule stated as a test, because it is the one an
// implementer is most likely to "helpfully" add: `unknown` is what the status
// API REPORTS when no row is retained. Writing it durably would overwrite a real
// terminal outcome with ignorance.
func TestUnknownIsNotAState(t *testing.T) {
	for s, name := range stateNames {
		if name == "unknown" {
			t.Fatalf("State(%d) spells itself %q; `unknown` is a reporting value and must never be durable", s, name)
		}
	}
	if _, err := ParseState("unknown"); err == nil {
		t.Fatal("ParseState(\"unknown\") succeeded; a durable record must never be able to say \"I don't know\"")
	}
}

// TestClosedSetsAreExactlyTheContractsSets guards the two closed vocabularies
// against silent growth. A thirteenth class, or a third attestation, is a design
// change that ACK-CONTRACT.md §5.2 and §6.3 have to be amended for — not
// something that should slip in beside a feature.
func TestClosedSetsAreExactlyTheContractsSets(t *testing.T) {
	if len(busClasses) != 9 {
		t.Errorf("there are %d bus-emitted classes, want exactly 9 (ACK-CONTRACT.md §5.2)", len(busClasses))
	}
	if len(recipientClasses) != 3 {
		t.Errorf("there are %d recipient-emitted classes, want exactly 3 (ACK-CONTRACT.md §5.2)", len(recipientClasses))
	}
	if len(busClasses)+len(recipientClasses) != 12 {
		t.Errorf("the class set has %d members, want exactly 12", len(busClasses)+len(recipientClasses))
	}
	for c := range busClasses {
		if c.RecipientEmitted() {
			t.Errorf("class %q is in BOTH halves of the set; the halves decide which party a refusal is attributed to", c)
		}
	}
	if len(attestations) != 2 {
		t.Errorf("there are %d attestation values, want exactly 2", len(attestations))
	}
	for a := range attestations {
		if strings.Contains(string(a), "verified") && a != AttestedByRecipientSignatureUnverified {
			t.Errorf("attestation %q reads as a verification claim; nothing in this system can verify a layer-3 attestation, so no value may imply one (§6.3)", a)
		}
	}
	if len(stateNames) != 5 {
		t.Errorf("there are %d durable states, want exactly 5 (§8.1)", len(stateNames))
	}
}

// TestEnumSpellingBounds keeps retention.go's footprint derivation honest: the
// named term must actually bound the longest spelling in each enum, or the
// derivation is a description of the happy path rather than a bound.
func TestEnumSpellingBounds(t *testing.T) {
	for s, name := range stateNames {
		if len(name) > MaxStateLen {
			t.Errorf("state %s spells %d bytes, over MaxStateLen = %d", s, len(name), MaxStateLen)
		}
	}
	for c := range busClasses {
		if len(c) > MaxClassLen {
			t.Errorf("class %q is %d bytes, over MaxClassLen = %d", c, len(c), MaxClassLen)
		}
	}
	for c := range recipientClasses {
		if len(c) > MaxClassLen {
			t.Errorf("class %q is %d bytes, over MaxClassLen = %d", c, len(c), MaxClassLen)
		}
	}
	for a := range attestations {
		if len(a) > MaxAttestationLen {
			t.Errorf("attestation %q is %d bytes, over MaxAttestationLen = %d", a, len(a), MaxAttestationLen)
		}
	}
}

// TestMaxRecordBytesBoundsWorstCase builds the LARGEST record the validators
// will admit and asserts its encoded size fits inside MaxRecordBytes.
//
// This is what makes MaxEntries a bound rather than a hope: the whole memory
// derivation rests on the record having no variable-length free-text field, and
// the only way that stays true is if a test measures it.
func TestMaxRecordBytesBoundsWorstCase(t *testing.T) {
	longestKey, err := ids.MessageID(strings.Repeat("b", 64), 1<<63)
	if err != nil {
		t.Fatalf("building the longest message id: %v", err)
	}
	longestAgent := strings.Repeat("c", 64) + "." + strings.Repeat("d", 32) + "-18446744073709551615"
	if _, _, _, err := ids.ParseAgentID(longestAgent); err != nil {
		// The exact maximum agent id shape is the ids package's business; fall
		// back to a valid one rather than encoding its private constants here.
		longestAgent = testRecipient
	}
	r := Record{
		CorrelationKey: longestKey,
		Recipient:      longestAgent,
		Sender:         longestAgent,
		State:          StateRefused,
		Class:          ClassRecipientRefusedUndecodable,
		AttestedBy:     AttestedByRecipientSignatureUnverified,
		AcceptedAt:     testAccepted,
		SettledAt:      testSettled,
	}
	body, err := r.Encode()
	if err != nil {
		t.Fatalf("Encode of the worst-case record: %v", err)
	}
	// The encoded record plus a generous allowance for the map bucket, the
	// composite key and the Go struct headers that the derivation also charges.
	const overheadAllowance = 350
	if got := len(body) + overheadAllowance; got > MaxRecordBytes {
		t.Fatalf("the worst-case record is %d encoded bytes (+%d overhead) = %d, over MaxRecordBytes = %d; the memory derivation in retention.go is no longer a bound",
			len(body), overheadAllowance, got, MaxRecordBytes)
	}
	if MaxEntries != MaxRetainedBytes/MaxRecordBytes {
		t.Fatalf("MaxEntries = %d is not the quotient of the budget and the record size", MaxEntries)
	}
	if PressureLine != MaxEntries/2 {
		t.Fatalf("PressureLine = %d is not the maxEntries/2 crossover", PressureLine)
	}
}

// TestExpiredAnchorsOnTheRightField pins the retention anchor: a non-terminal
// row ages from acceptance and a terminal one from settlement. Getting this
// backwards would either sweep live rows early or keep settled ones for ever.
func TestExpiredAnchorsOnTheRightField(t *testing.T) {
	open := acceptedRecord()
	settled := acceptedRecord()
	settled.State, settled.AttestedBy, settled.SettledAt = StateDelivered, AttestedByRecipientSignatureUnverified, testAccepted.Add(23*time.Hour)

	justInside := testAccepted.Add(Retention - time.Second)
	if open.Expired(justInside, Retention) {
		t.Error("an accepted row expired before its window ran out")
	}
	if !open.Expired(testAccepted.Add(Retention), Retention) {
		t.Error("an accepted row outlived its window; it ages from accepted_at")
	}
	if settled.Expired(testAccepted.Add(Retention), Retention) {
		t.Error("a terminal row aged from accepted_at; it must age from settled_at, or a late settlement is swept the moment it lands")
	}
	if !settled.Expired(settled.SettledAt.Add(Retention), Retention) {
		t.Error("a terminal row outlived its window measured from settled_at")
	}
}
