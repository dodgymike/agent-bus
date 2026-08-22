package auth_test

// AUTH-3, part 1: the DURABLE ENROLMENT RECORD.
//
// These tests pin the on-disk shape of an enrolment, because that shape is
// forever: the record goes into an append-only log that later builds must still
// read, and an agent id is bound to a keypair, so a migration here is not a
// schema edit but a forced re-enrolment of every agent (see record.go).
//
// Two things are asserted that a struct-level round trip alone would miss:
//
//   - the RAW JSON BYTES. "nil RetiredAt survives" is provable from the decoded
//     struct, but "a LIVE binding writes no retired_at key at all" is a claim
//     about the file, and only the bytes can answer it. Likewise the reserved
//     fields (msg_pub, invite_id, cert_bindings) being ABSENT rather than
//     null/empty is what makes today's record byte-identical to a pre-INVITE,
//     pre-MTLS one.
//   - every rejection with errors.Is against the sentinel, NEVER against error
//     text. The text is documented as diagnostic detail free to change.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// recordEpoch is the fixed instant every durable-record test stamps its entries
// with. Fixed and whole-second so the RFC3339Nano rendering is predictable
// enough to assert on byte for byte.
var recordEpoch = time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)

// recordEpochText is what recordEpoch renders as. RFC3339Nano drops trailing
// zeros in the fractional part, so a whole second carries none at all.
const recordEpochText = "2026-08-02T12:00:00Z"

// fixedKey returns a deterministic ed25519.PublicKeySize key. Deterministic,
// not random, because several assertions here are on the exact bytes of the
// encoded record and a fresh key would make them unrepeatable. Nothing here
// verifies a signature, so a key that is not on the curve is fine — the record
// carries public key BYTES and Decode's job is to check their LENGTH.
func fixedKey(b byte) ed25519.PublicKey {
	k := make([]byte, ed25519.PublicKeySize)
	for i := range k {
		k[i] = b
	}
	return ed25519.PublicKey(k)
}

// distinctAuthKey returns a deterministic ed25519.PublicKeySize key that is
// UNIQUE per agent id. baseEntry uses it so that two distinct agents never share
// an auth key by fixture accident — which the AUTH-DUP-ENROL-KEY uniqueness rule
// (Roster.Put rule 3) now refuses, exactly as real agents never share a keypair.
// Deterministic (a hash of the id, not random) so byte-exact record assertions
// stay repeatable; nothing here verifies a signature, so a key that is not on the
// curve is fine — Decode's job is to check the LENGTH.
func distinctAuthKey(agentID string) ed25519.PublicKey {
	sum := sha256.Sum256([]byte("auth-key:" + agentID))
	return ed25519.PublicKey(sum[:])
}

// fixedFingerprint returns a deterministic 32-byte certificate fingerprint.
func fixedFingerprint(b byte) [32]byte {
	var fp [32]byte
	for i := range fp {
		fp[i] = b
	}
	return fp
}

// mustAgentID builds the fully-qualified "<testBusID>.<name>-<n>" id
// (invariant 2) through the real formatter, so a test can never assert against
// a hand-spelled id the server would not have minted.
func mustAgentID(t *testing.T, name string, n uint64) string {
	t.Helper()
	id, err := ids.AgentID(testBusID, name, n)
	if err != nil {
		t.Fatalf("building agent id for %q-%d: %v", name, n, err)
	}
	return id
}

// baseEntry is the MINIMAL storable RosterEntry: a server-minted id, its name
// half, an auth key, and the two timestamps Decode refuses to do without. Every
// reserved field is left at its zero value, which is the reserved state.
func baseEntry(t *testing.T, name string, n uint64) auth.RosterEntry {
	t.Helper()
	id := mustAgentID(t, name, n)
	return auth.RosterEntry{
		AgentID: id,
		Name:    name,
		// Distinct per id (AUTH-DUP-ENROL-KEY): two agents built by this helper
		// must not collide on the auth-key uniqueness rule.
		AuthPublicKey: distinctAuthKey(id),
		Epoch:         recordEpoch,
		EnrolledAt:    recordEpoch,
	}
}

// normaliseEntry puts every time in an entry into UTC, so an entry built by a
// test and an entry decoded off disk are compared on the same footing. Decode
// stores UTC; a caller may legitimately have passed a local-zone time.
func normaliseEntry(e auth.RosterEntry) auth.RosterEntry {
	out := e
	out.Epoch = e.Epoch.UTC()
	out.EnrolledAt = e.EnrolledAt.UTC()
	if e.CertBindings != nil {
		out.CertBindings = make([]auth.CertBinding, len(e.CertBindings))
		for i, b := range e.CertBindings {
			nb := auth.CertBinding{Fingerprint: b.Fingerprint, BoundAt: b.BoundAt.UTC()}
			if b.RetiredAt != nil {
				u := b.RetiredAt.UTC()
				nb.RetiredAt = &u
			}
			out.CertBindings[i] = nb
		}
	}
	return out
}

// retiredAt is a pointer helper: a LIVE binding carries nil here, and nil is
// the only way to say "not retired" (rule 3 of ENROL-SHAPE).
func retiredAt(t time.Time) *time.Time { return &t }

// TestRecordRoundTrip is the exact-round-trip table: everything that goes into
// Encode comes back out of Decode unchanged, for every combination of populated
// and reserved fields the record is shaped to carry.
//
// The wantHas/wantLacks columns are the RAW-BYTES half of the contract and are
// the reason this is not just a reflect.DeepEqual loop — see the file comment.
func TestRecordRoundTrip(t *testing.T) {
	mixed := baseEntry(t, "worker", 4)
	mixed.CertBindings = []auth.CertBinding{
		{Fingerprint: fixedFingerprint(0xAA), BoundAt: recordEpoch},
		{Fingerprint: fixedFingerprint(0xBB), BoundAt: recordEpoch, RetiredAt: retiredAt(recordEpoch.Add(time.Hour))},
		{Fingerprint: fixedFingerprint(0xCC), BoundAt: recordEpoch.Add(2 * time.Hour)},
	}

	full := baseEntry(t, "worker", 5)
	full.MessagingPublicKey = fixedKey(0x33)
	full.InviteID = "invite-abc"
	full.CertBindings = make([]auth.CertBinding, auth.MaxCertBindings)
	for i := range full.CertBindings {
		full.CertBindings[i] = auth.CertBinding{
			Fingerprint: fixedFingerprint(byte(i)),
			BoundAt:     recordEpoch.Add(time.Duration(i) * time.Minute),
		}
	}

	live := baseEntry(t, "worker", 3)
	live.CertBindings = []auth.CertBinding{{Fingerprint: fixedFingerprint(0xDD), BoundAt: recordEpoch}}

	msgOnly := baseEntry(t, "worker", 1)
	msgOnly.MessagingPublicKey = fixedKey(0x22)

	inviteOnly := baseEntry(t, "worker", 2)
	inviteOnly.InviteID = "invite-7f3c"

	tests := []struct {
		name      string
		entry     auth.RosterEntry
		wantHas   []string
		wantLacks []string
	}{
		{
			name:  "minimal: every reserved field omitted",
			entry: baseEntry(t, "worker", 1),
			// The reserved keys must be ABSENT, not null and not empty: that is
			// what makes this record byte-identical to one a pre-INVITE,
			// pre-MTLS build would have written.
			wantLacks: []string{`"msg_pub"`, `"invite_id"`, `"cert_bindings"`, `"retired_at"`},
		},
		{
			name:      "messaging public key populated",
			entry:     msgOnly,
			wantHas:   []string{`"msg_pub":"` + base64.StdEncoding.EncodeToString(fixedKey(0x22)) + `"`},
			wantLacks: []string{`"invite_id"`, `"cert_bindings"`},
		},
		{
			name:      "invite id populated",
			entry:     inviteOnly,
			wantHas:   []string{`"invite_id":"invite-7f3c"`},
			wantLacks: []string{`"msg_pub"`, `"cert_bindings"`},
		},
		{
			name:    "one LIVE certificate binding",
			entry:   live,
			wantHas: []string{`"fp":"` + hex.EncodeToString(bytesOf(fixedFingerprint(0xDD))) + `"`, `"bound_at":"` + recordEpochText + `"`},
			// The single most load-bearing byte assertion in this file: a live
			// binding writes NO retired_at key, so "live" and "retired at the
			// zero time" can never be confused on disk.
			wantLacks: []string{`"retired_at"`},
		},
		{
			name:    "mixed live and retired bindings",
			entry:   mixed,
			wantHas: []string{`"retired_at":"2026-08-02T13:00:00Z"`},
		},
		{
			name:      "exactly MaxCertBindings bindings",
			entry:     full,
			wantHas:   []string{`"cert_bindings":[`, `"msg_pub":`, `"invite_id":"invite-abc"`},
			wantLacks: []string{`"retired_at"`},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			raw, err := auth.Encode(tc.entry)
			if err != nil {
				t.Fatalf("Encode(%+v) = %v, want a record", tc.entry, err)
			}
			got := string(raw)
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("the encoded record does not contain %s\n  record: %s", want, got)
				}
			}
			for _, unwanted := range tc.wantLacks {
				if strings.Contains(got, unwanted) {
					t.Errorf("the encoded record contains %s, which must be OMITTED while unpopulated;\nan absent key is what keeps this record byte-identical to a pre-INVITE/pre-MTLS one\n  record: %s", unwanted, got)
				}
			}

			back, err := auth.Decode(raw)
			if err != nil {
				t.Fatalf("Decode of the record just encoded = %v, want the entry back\n  record: %s", err, got)
			}
			want := normaliseEntry(tc.entry)
			if !reflect.DeepEqual(back, want) {
				t.Fatalf("the record did not round trip.\n  got  %+v\n  want %+v\n  record: %s", back, want, got)
			}
		})
	}
}

// bytesOf turns a fingerprint array into the slice hex.EncodeToString needs.
func bytesOf(fp [32]byte) []byte { return fp[:] }

// TestRecordRetiredAtNilVersusSetSurvives isolates the nil-vs-set distinction on
// RetiredAt, because the round-trip table would still pass if BOTH bindings came
// back live (or both retired) as long as they came back the same way.
//
// It asserts on the count of retired_at keys in the FILE as well as on the
// decoded pointers: retirement is explicit and an absent key is the only way to
// say "live", so one retired binding must produce exactly one key.
func TestRecordRetiredAtNilVersusSetSurvives(t *testing.T) {
	e := baseEntry(t, "worker", 9)
	retirement := recordEpoch.Add(90 * time.Minute)
	e.CertBindings = []auth.CertBinding{
		{Fingerprint: fixedFingerprint(0x01), BoundAt: recordEpoch},
		{Fingerprint: fixedFingerprint(0x02), BoundAt: recordEpoch, RetiredAt: retiredAt(retirement)},
		{Fingerprint: fixedFingerprint(0x03), BoundAt: recordEpoch},
	}

	raw, err := auth.Encode(e)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if n := strings.Count(string(raw), `"retired_at"`); n != 1 {
		t.Fatalf("the record carries %d retired_at keys, want exactly 1 (one of the three bindings is retired);\nthe two LIVE bindings must write no such key at all\n  record: %s", n, raw)
	}

	back, err := auth.Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(back.CertBindings) != 3 {
		t.Fatalf("decoded %d bindings, want 3", len(back.CertBindings))
	}
	if back.CertBindings[0].RetiredAt != nil {
		t.Errorf("binding 0 came back retired at %v, want LIVE (nil)", *back.CertBindings[0].RetiredAt)
	}
	if back.CertBindings[2].RetiredAt != nil {
		t.Errorf("binding 2 came back retired at %v, want LIVE (nil)", *back.CertBindings[2].RetiredAt)
	}
	if back.CertBindings[1].RetiredAt == nil {
		t.Fatalf("binding 1 came back LIVE, want retired at %v; a retirement that decodes as live is a revoked certificate the bus would go on accepting", retirement)
	}
	if !back.CertBindings[1].RetiredAt.Equal(retirement) {
		t.Errorf("binding 1 retired at %v, want %v", *back.CertBindings[1].RetiredAt, retirement)
	}
}

// TestRecordMinimalIsByteIdenticalToAPreInviteRecord pins the WHOLE encoding of
// a record that populates nothing reserved, key order included.
//
// This is the strongest form of the "adding the reserved fields cost nothing on
// disk" claim in record.go: today's encoder, given today's only populated
// fields, emits exactly the bytes a build with no msg_pub, no invite_id and no
// cert_bindings would have emitted.
func TestRecordMinimalIsByteIdenticalToAPreInviteRecord(t *testing.T) {
	raw, err := auth.Encode(baseEntry(t, "worker", 1))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := `{"v":1,"agent_id":"` + testBusID + `.worker-1","name":"worker","auth_pub":"` +
		base64.StdEncoding.EncodeToString(distinctAuthKey(testBusID+".worker-1")) + `","epoch":"` + recordEpochText +
		`","enrolled_at":"` + recordEpochText + `"}`
	if string(raw) != want {
		t.Fatalf("the on-disk record changed shape. THESE FIELD NAMES AND THIS ORDER ARE FOREVER.\n  got  %s\n  want %s", raw, want)
	}
}

// rawRecord builds a raw on-disk record from the valid baseline, letting the
// caller corrupt exactly one thing. Building it as a map rather than a string
// template keeps each rejection case to the single mutation it is about.
func rawRecord(t *testing.T, mutate func(map[string]interface{})) json.RawMessage {
	t.Helper()
	m := map[string]interface{}{
		"v":           auth.RecordVersion,
		"agent_id":    testBusID + ".worker-1",
		"name":        "worker",
		"auth_pub":    base64.StdEncoding.EncodeToString(fixedKey(0x11)),
		"epoch":       recordEpochText,
		"enrolled_at": recordEpochText,
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshalling the test record: %v", err)
	}
	return json.RawMessage(b)
}

// binding renders one on-disk certificate binding.
func binding(fp string, boundAt string) map[string]interface{} {
	return map[string]interface{}{"fp": fp, "bound_at": boundAt}
}

// TestDecodeRejects is the rejection table. A record read off disk is UNTRUSTED
// INPUT even though this server wrote it — "this server wrote it" is exactly the
// claim corruption disproves — so every one of these must be refused rather than
// interpreted.
//
// Every case is matched with errors.Is against auth.ErrInvalidRecord and never
// against error text.
func TestDecodeRejects(t *testing.T) {
	goodFP := hex.EncodeToString(bytesOf(fixedFingerprint(0x55)))

	tooMany := make([]map[string]interface{}, auth.MaxCertBindings+1)
	for i := range tooMany {
		tooMany[i] = binding(goodFP, recordEpochText)
	}

	tests := []struct {
		name string
		raw  json.RawMessage
	}{
		{
			name: "schema version this build does not understand",
			raw:  rawRecord(t, func(m map[string]interface{}) { m["v"] = 999 }),
		},
		{
			name: "unknown field",
			raw:  rawRecord(t, func(m map[string]interface{}) { m["revoked"] = true }),
		},
		{
			name: "trailing data after the record",
			raw:  append(rawRecord(t, nil), []byte(`{"v":1}`)...),
		},
		{
			name: "auth_pub of the wrong length",
			raw: rawRecord(t, func(m map[string]interface{}) {
				m["auth_pub"] = base64.StdEncoding.EncodeToString(fixedKey(0x11)[:ed25519.PublicKeySize-1])
			}),
		},
		{
			name: "auth_pub that is not base64",
			raw:  rawRecord(t, func(m map[string]interface{}) { m["auth_pub"] = "!!! not base64 !!!" }),
		},
		{
			name: "msg_pub of the wrong length",
			raw: rawRecord(t, func(m map[string]interface{}) {
				m["msg_pub"] = base64.StdEncoding.EncodeToString(append([]byte(fixedKey(0x22)), 0x00))
			}),
		},
		{
			name: "name disagreeing with the id's name half",
			raw:  rawRecord(t, func(m map[string]interface{}) { m["name"] = "worker-impostor" }),
		},
		{
			name: "unparseable agent_id: no bus qualification",
			raw:  rawRecord(t, func(m map[string]interface{}) { m["agent_id"] = "worker-1" }),
		},
		{
			name: "unparseable agent_id: suffix 0 is never allocated",
			raw: rawRecord(t, func(m map[string]interface{}) {
				m["agent_id"] = testBusID + ".worker-0"
			}),
		},
		{
			name: "empty agent_id",
			raw:  rawRecord(t, func(m map[string]interface{}) { m["agent_id"] = "" }),
		},
		{
			name: "MaxCertBindings+1 bindings",
			raw:  rawRecord(t, func(m map[string]interface{}) { m["cert_bindings"] = tooMany }),
		},
		{
			name: "fingerprint that is not hex",
			raw: rawRecord(t, func(m map[string]interface{}) {
				m["cert_bindings"] = []map[string]interface{}{binding("zzzz", recordEpochText)}
			}),
		},
		{
			name: "fingerprint of the wrong length",
			raw: rawRecord(t, func(m map[string]interface{}) {
				m["cert_bindings"] = []map[string]interface{}{binding(goodFP[:len(goodFP)-2], recordEpochText)}
			}),
		},
		{
			name: "retired_at that is not RFC3339Nano",
			raw: rawRecord(t, func(m map[string]interface{}) {
				b := binding(goodFP, recordEpochText)
				b["retired_at"] = "yesterday"
				m["cert_bindings"] = []map[string]interface{}{b}
			}),
		},
		{
			name: "bound_at that is not RFC3339Nano",
			raw: rawRecord(t, func(m map[string]interface{}) {
				m["cert_bindings"] = []map[string]interface{}{binding(goodFP, "2026-08-02 12:00:00")}
			}),
		},
		{
			name: "epoch that is not RFC3339Nano",
			raw:  rawRecord(t, func(m map[string]interface{}) { m["epoch"] = "" }),
		},
		{
			name: "enrolled_at that is not RFC3339Nano",
			raw:  rawRecord(t, func(m map[string]interface{}) { m["enrolled_at"] = "0" }),
		},
		{
			name: "not JSON at all",
			raw:  json.RawMessage(`not json`),
		},
		{
			name: "empty body",
			raw:  json.RawMessage(``),
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := auth.Decode(tc.raw)
			if err == nil {
				t.Fatalf("Decode(%s) = %+v with no error; this record must be REFUSED, not interpreted", tc.raw, got)
			}
			if !errors.Is(err, auth.ErrInvalidRecord) {
				t.Fatalf("Decode(%s) = %v, want an error satisfying errors.Is(err, auth.ErrInvalidRecord)", tc.raw, err)
			}
			if !reflect.DeepEqual(got, auth.RosterEntry{}) {
				t.Errorf("Decode returned a partially-populated entry %+v alongside its error; a refused record must yield the zero entry", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The OTHER direction: validateRosterEntry on the way OUT.
//
// The rejection table above only exercises Decode, i.e. bytes coming OFF disk.
// Deleting validateRosterEntry from Encode and from WALRoster.Put left that
// table entirely green, because a bad entry still fails at the NEXT restart
// when Decode reads it back — by which time the enrolment is durable, the
// client has been told its agent id, and every remaining option is bad. The two
// tests below pin the out-bound direction so that mutation goes red where it
// matters: with NOTHING written.
// ---------------------------------------------------------------------------

// unstorableEntry is one RosterEntry that validateRosterEntry must refuse, and
// the sentinel it must be refused with. Note the sentinel is NOT uniformly
// ErrInvalidRecord: a wrong-size key is ErrInvalidPublicKey, because that is
// the length check standing between untrusted bytes and an ed25519.Verify that
// PANICS on a wrong-size key.
type unstorableEntry struct {
	name     string
	entry    auth.RosterEntry
	sentinel error
}

// unstorableEntries is the shared table behind TestEncodeRefusesAnUnstorableEntry
// and TestWALRosterPutRefusesAnUnstorableEntryBeforeWriting. It is shared
// deliberately: Encode and Put must apply the SAME predicate, and two tables
// would let one call site quietly lose a case.
func unstorableEntries(t *testing.T) []unstorableEntry {
	t.Helper()

	nameDisagrees := baseEntry(t, "worker", 1)
	nameDisagrees.Name = "impostor"

	shortAuthKey := baseEntry(t, "worker", 1)
	shortAuthKey.AuthPublicKey = fixedKey(0x11)[:ed25519.PublicKeySize-1]

	noAuthKey := baseEntry(t, "worker", 1)
	noAuthKey.AuthPublicKey = nil

	longMsgKey := baseEntry(t, "worker", 1)
	longMsgKey.MessagingPublicKey = append(fixedKey(0x22), 0x00)

	shortMsgKey := baseEntry(t, "worker", 1)
	shortMsgKey.MessagingPublicKey = fixedKey(0x22)[:ed25519.PublicKeySize-1]

	zeroEpoch := baseEntry(t, "worker", 1)
	zeroEpoch.Epoch = time.Time{}

	zeroEnrolledAt := baseEntry(t, "worker", 1)
	zeroEnrolledAt.EnrolledAt = time.Time{}

	tooManyBindings := baseEntry(t, "worker", 1)
	tooManyBindings.CertBindings = make([]auth.CertBinding, auth.MaxCertBindings+1)
	for i := range tooManyBindings.CertBindings {
		tooManyBindings.CertBindings[i] = auth.CertBinding{
			Fingerprint: fixedFingerprint(byte(i)),
			BoundAt:     recordEpoch,
		}
	}

	zeroBoundAt := baseEntry(t, "worker", 1)
	zeroBoundAt.CertBindings = []auth.CertBinding{
		{Fingerprint: fixedFingerprint(0x55), BoundAt: recordEpoch},
		{Fingerprint: fixedFingerprint(0x66)}, // BoundAt left at the zero time
	}

	zeroRetiredAt := baseEntry(t, "worker", 1)
	zeroRetiredAt.CertBindings = []auth.CertBinding{
		{Fingerprint: fixedFingerprint(0x77), BoundAt: recordEpoch, RetiredAt: retiredAt(time.Time{})},
	}

	unparseableID := baseEntry(t, "worker", 1)
	unparseableID.AgentID = "worker-1" // no bus qualification (invariant 2)

	emptyID := baseEntry(t, "worker", 1)
	emptyID.AgentID = ""

	return []unstorableEntry{
		{"name disagreeing with the agent id", nameDisagrees, auth.ErrInvalidRecord},
		{"auth public key one byte short", shortAuthKey, auth.ErrInvalidPublicKey},
		{"no auth public key at all", noAuthKey, auth.ErrInvalidPublicKey},
		{"messaging public key one byte long", longMsgKey, auth.ErrInvalidPublicKey},
		{"messaging public key one byte short", shortMsgKey, auth.ErrInvalidPublicKey},
		{"zero epoch", zeroEpoch, auth.ErrInvalidRecord},
		{"zero enrolled_at", zeroEnrolledAt, auth.ErrInvalidRecord},
		{"MaxCertBindings+1 certificate bindings", tooManyBindings, auth.ErrInvalidRecord},
		{"a certificate binding with a zero bound_at", zeroBoundAt, auth.ErrInvalidRecord},
		{"a certificate binding retired at the zero time", zeroRetiredAt, auth.ErrInvalidRecord},
		{"an agent id that does not parse", unparseableID, auth.ErrInvalidRecord},
		{"an empty agent id", emptyID, auth.ErrInvalidRecord},
	}
}

// TestEncodeRefusesAnUnstorableEntry: Encode VALIDATES BEFORE IT RETURNS, so a
// record that cannot be stored fails the whole operation with nothing written.
//
// The nil-bytes assertion is the load-bearing one. A caller that got bytes back
// alongside an error — and this package's caller is WALRoster.Put, which hands
// them straight to an fsync — would durably record the very entry Encode
// refused.
func TestEncodeRefusesAnUnstorableEntry(t *testing.T) {
	for _, tc := range unstorableEntries(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			raw, err := auth.Encode(tc.entry)
			if err == nil {
				t.Fatalf("Encode(%+v) returned %s with NO error; validation runs before the durable write precisely so an unstorable record fails with nothing written", tc.entry, raw)
			}
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("Encode = %v, want an error satisfying errors.Is(err, %v)", err, tc.sentinel)
			}
			if raw != nil {
				t.Errorf("Encode returned %d bytes (%s) alongside its error, want nil; its caller writes whatever it is handed", len(raw), raw)
			}
		})
	}
}

// TestDecodeAcceptsAnEmptyCertBindingsArray covers the one shape the rejection
// table would otherwise leave ambiguous: an explicitly empty array is legal and
// decodes to a nil slice, so it cannot be confused with a bounds violation.
func TestDecodeAcceptsAnEmptyCertBindingsArray(t *testing.T) {
	raw := rawRecord(t, func(m map[string]interface{}) {
		m["cert_bindings"] = []map[string]interface{}{}
	})
	got, err := auth.Decode(raw)
	if err != nil {
		t.Fatalf("Decode of a record with an empty cert_bindings array = %v, want it accepted", err)
	}
	if got.CertBindings != nil {
		t.Errorf("cert_bindings decoded to %#v, want nil: an empty history and an absent one are the same state", got.CertBindings)
	}
}
