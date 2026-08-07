// These tests exercise IDEM-11's exported surface only (retention constants,
// Record.Encode/DecodeRecord/Scope, Store), matching on errors.Is against a
// sentinel and never on error text — the same posture idem_test.go uses.
package idem_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
)

const testAgent = "bus1.alice-1"

// mustRecord builds a valid per-agent Record at t, for the tests that care
// about the Store rather than about record validation.
func mustRecord(t *testing.T, key string, fp idem.Fingerprint, at time.Time) idem.Record {
	t.Helper()
	return idem.Record{
		Agent:       testAgent,
		Op:          idem.OpSend,
		Key:         key,
		Fingerprint: fp,
		Result:      json.RawMessage(`{"message_id":"bus1-7"}`),
		Seq:         7,
		CommittedAt: at,
	}
}

func fp(b byte) idem.Fingerprint {
	var f idem.Fingerprint
	f[0] = b
	return f
}

// TestRetentionWindowDerivation pins the derived window so it cannot drift
// silently. If a term in retention.go changes, this fails and the new total has
// to be stated deliberately rather than absorbed.
func TestRetentionWindowDerivation(t *testing.T) {
	tests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"peer outage budget", idem.PeerOutageBudget, 24 * time.Hour},
		{"session lifetime max (invariant 3)", idem.SessionLifetimeMax, time.Hour},
		{"parked poll max (hub.MaxPollTimeout)", idem.ParkedPollMax, 5 * time.Minute},
		{"transport retry horizon", idem.TransportRetryHorizon, 11 * time.Second},
		{"max retry horizon", idem.MaxRetryHorizon, 25*time.Hour + 5*time.Minute + 11*time.Second},
		{"retention window", idem.RetentionWindow, 50*time.Hour + 10*time.Minute + 22*time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("= %v, want %v", tc.got, tc.want)
			}
		})
	}
	if idem.RetentionWindow != idem.MaxRetryHorizon*idem.RetentionSafetyFactor {
		t.Fatalf("RetentionWindow %v is not MaxRetryHorizon %v x %d",
			idem.RetentionWindow, idem.MaxRetryHorizon, idem.RetentionSafetyFactor)
	}
}

// TestMemoryBoundDerivation pins the memory bound, including the one number
// that is also written down in CONTRACTS-HTTP.md.
func TestMemoryBoundDerivation(t *testing.T) {
	if idem.MaxEntries != 65536 {
		t.Fatalf("MaxEntries = %d, want 65536 (the value CONTRACTS-HTTP.md documents for hub.MaxIdempotencyEntries)", idem.MaxEntries)
	}
	if idem.MaxEntries != idem.MaxRetainedBytes/idem.MaxRecordBytes {
		t.Fatalf("MaxEntries %d is not MaxRetainedBytes %d / MaxRecordBytes %d",
			idem.MaxEntries, idem.MaxRetainedBytes, idem.MaxRecordBytes)
	}
}

// TestRecordRoundTrip proves a record survives encode/decode byte-for-byte in
// meaning, for both scope shapes and for the omitted-field cases.
func TestRecordRoundTrip(t *testing.T) {
	at := time.Date(2026, 8, 2, 12, 0, 0, 123456789, time.UTC)
	tests := []struct {
		name string
		rec  idem.Record
		want string
	}{
		{
			name: "per-agent send with a result",
			rec: idem.Record{
				Agent: testAgent, Op: idem.OpSend, Key: "k1",
				Fingerprint: fp(0xab),
				Result:      json.RawMessage(`{"message_id":"bus1-7","seq":7}`),
				Seq:         7, CommittedAt: at,
			},
			want: `{"agent":"bus1.alice-1","op":"send","key":"k1","fp":"ab` + strings.Repeat("00", 31) + `","result":{"message_id":"bus1-7","seq":7},"seq":7,"committed_at":"2026-08-02T12:00:00.123456789Z"}`,
		},
		{
			name: "bus-wide enrol, no result, no seq",
			rec: idem.Record{
				EnrolBusWide: true, Op: idem.OpEnrol, Key: "k2",
				Fingerprint: fp(0x01), CommittedAt: at,
			},
			want: `{"enrol_bus_wide":true,"op":"enrol","key":"k2","fp":"01` + strings.Repeat("00", 31) + `","committed_at":"2026-08-02T12:00:00.123456789Z"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc, err := tc.rec.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if string(enc) != tc.want {
				t.Fatalf("Encode wrote\n %s\nwant\n %s", enc, tc.want)
			}
			back, err := idem.DecodeRecord(enc)
			if err != nil {
				t.Fatalf("DecodeRecord: %v", err)
			}
			if back.Agent != tc.rec.Agent || back.EnrolBusWide != tc.rec.EnrolBusWide ||
				back.Op != tc.rec.Op || back.Key != tc.rec.Key ||
				back.Fingerprint != tc.rec.Fingerprint || back.Seq != tc.rec.Seq ||
				!back.CommittedAt.Equal(tc.rec.CommittedAt) {
				t.Fatalf("round trip changed the record:\n got %+v\nwant %+v", back, tc.rec)
			}
			// The scope must rebuild through the constructors.
			gotSc, err := back.Scope()
			if err != nil {
				t.Fatalf("Scope: %v", err)
			}
			wantSc, err := tc.rec.Scope()
			if err != nil {
				t.Fatalf("Scope (original): %v", err)
			}
			if gotSc != wantSc {
				t.Fatalf("rebuilt scope %+v, want %+v", gotSc, wantSc)
			}
		})
	}
}

// TestRecordEncodeRejects proves Encode validates BEFORE it returns, so a
// record that cannot be stored fails the operation with nothing written rather
// than being discovered at replay.
func TestRecordEncodeRejects(t *testing.T) {
	at := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		rec  idem.Record
		want error
	}{
		{
			name: "unknown operation",
			rec:  idem.Record{Agent: testAgent, Op: "teleport", Key: "k", CommittedAt: at},
			want: idem.ErrInvalidRecord,
		},
		{
			name: "invalid key",
			rec:  idem.Record{Agent: testAgent, Op: idem.OpSend, Key: "bad key", CommittedAt: at},
			want: idem.ErrInvalidRecord,
		},
		{
			name: "empty key",
			rec:  idem.Record{Agent: testAgent, Op: idem.OpSend, Key: "", CommittedAt: at},
			want: idem.ErrInvalidRecord,
		},
		{
			name: "agent empty and not bus-wide",
			rec:  idem.Record{Op: idem.OpSend, Key: "k", CommittedAt: at},
			want: idem.ErrInvalidRecord,
		},
		{
			// The memory derivation in retention.go counts the agent field at
			// MaxAgentLen. This is the check that makes that an ENFORCED bound
			// rather than a description of the happy path: without it a record
			// from a damaged or hostile log could carry an agent field limited
			// only by wal.MaxPayloadSize (1 MiB), and MaxEntries of those would
			// be three orders of magnitude past the 64 MiB budget MaxEntries is
			// derived from.
			name: "agent id over MaxAgentLen",
			rec: idem.Record{
				Agent:       strings.Repeat("a", idem.MaxAgentLen+1),
				Op:          idem.OpSend,
				Key:         "k",
				CommittedAt: at,
			},
			want: idem.ErrInvalidRecord,
		},
		{
			name: "agent set on a bus-wide record",
			rec:  idem.Record{Agent: testAgent, EnrolBusWide: true, Op: idem.OpEnrol, Key: "k", CommittedAt: at},
			want: idem.ErrInvalidRecord,
		},
		{
			name: "enrol without the bus-wide discriminant has no constructible scope",
			rec:  idem.Record{Agent: testAgent, Op: idem.OpEnrol, Key: "k", CommittedAt: at},
			want: idem.ErrInvalidRecord,
		},
		{
			name: "zero commit time",
			rec:  idem.Record{Agent: testAgent, Op: idem.OpSend, Key: "k"},
			want: idem.ErrInvalidRecord,
		},
		{
			name: "result over MaxResultBytes",
			rec: idem.Record{
				Agent: testAgent, Op: idem.OpSend, Key: "k", CommittedAt: at,
				Result: json.RawMessage(`"` + strings.Repeat("x", idem.MaxResultBytes) + `"`),
			},
			want: idem.ErrResultTooLarge,
		},
		{
			name: "result that is not JSON",
			rec: idem.Record{
				Agent: testAgent, Op: idem.OpSend, Key: "k", CommittedAt: at,
				Result: json.RawMessage(`{oops`),
			},
			want: idem.ErrInvalidRecord,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.rec.Encode()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Encode: err = %v, want %v", err, tc.want)
			}
			if got != nil {
				t.Fatalf("a rejected record still encoded to %s", got)
			}
		})
	}
}

// TestMaxAgentLenBoundaryIsExact pins the agent-length bound at exactly
// MaxAgentLen rather than one byte either side of it.
//
// The over-length case is in TestRecordEncodeRejects; this is its other half.
// Both are needed: a bound that only rejects proves nothing about where it sits,
// and an off-by-one here would reject the longest LEGITIMATE agent id
// (ids.MaxAgentIDLen) — which on the replay path silently drops that agent's
// applied keys and lets a later retry apply twice.
func TestMaxAgentLenBoundaryIsExact(t *testing.T) {
	at := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	rec := idem.Record{
		Agent:       strings.Repeat("a", idem.MaxAgentLen),
		Op:          idem.OpSend,
		Key:         "k",
		CommittedAt: at,
	}
	got, err := rec.Encode()
	if err != nil {
		t.Fatalf("an agent id of exactly MaxAgentLen (%d) must encode, got %v", idem.MaxAgentLen, err)
	}
	if got == nil {
		t.Fatal("an accepted record encoded to nil")
	}
	back, err := idem.DecodeRecord(got)
	if err != nil {
		t.Fatalf("an agent id of exactly MaxAgentLen must decode back, got %v", err)
	}
	if len(back.Agent) != idem.MaxAgentLen {
		t.Fatalf("round trip changed the agent id length: %d, want %d", len(back.Agent), idem.MaxAgentLen)
	}
}

// TestDecodeRecordIsStrict proves a record read off disk is treated as
// untrusted input (invariant 1): unknown fields, trailing data and malformed
// values are refused rather than half-accepted.
func TestDecodeRecordIsStrict(t *testing.T) {
	good := `{"agent":"bus1.alice-1","op":"send","key":"k1","fp":"` + strings.Repeat("ab", 32) + `","committed_at":"2026-08-02T12:00:00Z"}`
	if _, err := idem.DecodeRecord([]byte(good)); err != nil {
		t.Fatalf("the control case must decode, got %v", err)
	}
	tests := []struct {
		name string
		in   string
	}{
		{"unknown field", `{"agent":"bus1.alice-1","op":"send","key":"k1","fp":"` + strings.Repeat("ab", 32) + `","committed_at":"2026-08-02T12:00:00Z","extra":1}`},
		{"trailing data", good + `{"another":1}`},
		{"fingerprint not hex", `{"agent":"bus1.alice-1","op":"send","key":"k1","fp":"zz","committed_at":"2026-08-02T12:00:00Z"}`},
		{"fingerprint wrong length", `{"agent":"bus1.alice-1","op":"send","key":"k1","fp":"abcd","committed_at":"2026-08-02T12:00:00Z"}`},
		{"committed_at not RFC3339Nano", `{"agent":"bus1.alice-1","op":"send","key":"k1","fp":"` + strings.Repeat("ab", 32) + `","committed_at":"yesterday"}`},
		{"unknown operation", `{"agent":"bus1.alice-1","op":"teleport","key":"k1","fp":"` + strings.Repeat("ab", 32) + `","committed_at":"2026-08-02T12:00:00Z"}`},
		{"invalid key", `{"agent":"bus1.alice-1","op":"send","key":"bad key","fp":"` + strings.Repeat("ab", 32) + `","committed_at":"2026-08-02T12:00:00Z"}`},
		{"no agent and not bus-wide", `{"op":"send","key":"k1","fp":"` + strings.Repeat("ab", 32) + `","committed_at":"2026-08-02T12:00:00Z"}`},
		{"empty", ``},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := idem.DecodeRecord([]byte(tc.in)); !errors.Is(err, idem.ErrInvalidRecord) {
				t.Fatalf("DecodeRecord: err = %v, want ErrInvalidRecord", err)
			}
		})
	}
}

// TestStoreOutcomes is the three-way split invariant 10 turns on. Collapsing it
// to a bool is what this test exists to prevent.
func TestStoreOutcomes(t *testing.T) {
	at := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	now := at
	s := idem.NewStore(idem.StoreOptions{Now: func() time.Time { return now }})

	sc, err := idem.NewAgentScope(testAgent, idem.OpSend, "k1")
	if err != nil {
		t.Fatalf("NewAgentScope: %v", err)
	}

	if _, out := s.Lookup(sc, fp(1)); out != idem.OutcomeNew {
		t.Fatalf("an unseen key is %v, want OutcomeNew", out)
	}
	if err := s.Remember(mustRecord(t, "k1", fp(1), at)); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	rec, out := s.Lookup(sc, fp(1))
	if out != idem.OutcomeRetry {
		t.Fatalf("same key + same payload is %v, want OutcomeRetry", out)
	}
	if string(rec.Result) != `{"message_id":"bus1-7"}` {
		t.Fatalf("a retry must return the ORIGINAL result, got %s", rec.Result)
	}
	if _, out := s.Lookup(sc, fp(2)); out != idem.OutcomeViolation {
		t.Fatalf("same key + different payload is %v, want OutcomeViolation", out)
	}
	// A different agent's identical key is a DIFFERENT scope: no collision, no
	// probing (doc.go point 3).
	other, err := idem.NewAgentScope("bus1.bob-2", idem.OpSend, "k1")
	if err != nil {
		t.Fatalf("NewAgentScope: %v", err)
	}
	if _, out := s.Lookup(other, fp(2)); out != idem.OutcomeNew {
		t.Fatalf("another agent's identical key is %v, want OutcomeNew", out)
	}
	// And a different OPERATION with the same key is also a different scope.
	crossOp, err := idem.NewAgentScope(testAgent, idem.OpBroadcast, "k1")
	if err != nil {
		t.Fatalf("NewAgentScope: %v", err)
	}
	if _, out := s.Lookup(crossOp, fp(2)); out != idem.OutcomeNew {
		t.Fatalf("the same key on another operation is %v, want OutcomeNew", out)
	}
}

// TestStoreRememberIsIdempotent proves a repeated Remember of the same scope is
// a no-op that keeps the FIRST record — which is what makes replay safe to run
// twice and a live double-remember harmless rather than corrupting.
func TestStoreRememberIsIdempotent(t *testing.T) {
	at := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	s := idem.NewStore(idem.StoreOptions{Now: func() time.Time { return at }})
	first := mustRecord(t, "k1", fp(1), at)
	second := mustRecord(t, "k1", fp(9), at)
	second.Result = json.RawMessage(`{"message_id":"bus1-99"}`)

	if err := s.Remember(first); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := s.Remember(second); err != nil {
		t.Fatalf("a duplicate Remember must be a no-op returning nil, got %v", err)
	}
	if got := s.Stats().Count; got != 1 {
		t.Fatalf("Count = %d, want 1", got)
	}
	sc, err := idem.NewAgentScope(testAgent, idem.OpSend, "k1")
	if err != nil {
		t.Fatalf("NewAgentScope: %v", err)
	}
	rec, out := s.Lookup(sc, fp(1))
	if out != idem.OutcomeRetry || string(rec.Result) != `{"message_id":"bus1-7"}` {
		t.Fatalf("the FIRST record must win: outcome %v result %s", out, rec.Result)
	}
}

// TestStoreExpiry proves the retention window is a pure predicate over
// CommittedAt, that expiry stops at the first live record, and that an expired
// key is applied as a NEW operation — the honest bounded-window guarantee,
// tested rather than merely documented.
func TestStoreExpiry(t *testing.T) {
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	now := base
	window := time.Hour
	s := idem.NewStore(idem.StoreOptions{Window: window, Now: func() time.Time { return now }})

	for i, key := range []string{"k1", "k2", "k3"} {
		if err := s.Remember(mustRecord(t, key, fp(byte(i+1)), base.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatalf("Remember %s: %v", key, err)
		}
	}
	if got := s.Stats().Count; got != 3 {
		t.Fatalf("Count = %d, want 3", got)
	}

	// Exactly at the window: still live (the predicate is `>`, not `>=`).
	now = base.Add(window)
	st := s.Stats()
	if st.Count != 3 || st.Expired != 0 {
		t.Fatalf("at exactly the window: Count %d Expired %d, want 3 and 0", st.Count, st.Expired)
	}
	if st.OldestAge != window || !st.Oldest.Equal(base) {
		t.Fatalf("Oldest = %v (age %v), want %v (age %v)", st.Oldest, st.OldestAge, base, window)
	}

	// One nanosecond past it, only the front record goes.
	now = base.Add(window + time.Nanosecond)
	st = s.Stats()
	if st.Count != 2 || st.Expired != 1 {
		t.Fatalf("one ns past the window: Count %d Expired %d, want 2 and 1", st.Count, st.Expired)
	}

	// The evicted key is now indistinguishable from a never-seen key: it is
	// applied as a NEW operation. THIS IS THE DOCUMENTED BOUNDARY, not a bug.
	sc, err := idem.NewAgentScope(testAgent, idem.OpSend, "k1")
	if err != nil {
		t.Fatalf("NewAgentScope: %v", err)
	}
	if _, out := s.Lookup(sc, fp(1)); out != idem.OutcomeNew {
		t.Fatalf("a key past its window is %v, want OutcomeNew (duplicates are suppressed WITHIN the window only)", out)
	}

	// Everything past the window goes; the reported bounds do not move.
	now = base.Add(24 * time.Hour)
	st = s.Stats()
	if st.Count != 0 || st.Expired != 3 {
		t.Fatalf("well past the window: Count %d Expired %d, want 0 and 3", st.Count, st.Expired)
	}
	if !st.Oldest.IsZero() || st.OldestAge != 0 {
		t.Fatalf("an empty table must report a zero Oldest, got %v / %v", st.Oldest, st.OldestAge)
	}
	if st.Window != window || st.MaxEntries != idem.MaxEntries {
		t.Fatalf("Stats bounds = %v / %d, want %v / %d", st.Window, st.MaxEntries, window, idem.MaxEntries)
	}
}

// TestStoreCapacityFailsClosed proves the table refuses rather than evicting a
// live key, and that expiry reclaims the room instead.
func TestStoreCapacityFailsClosed(t *testing.T) {
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	now := base
	s := idem.NewStore(idem.StoreOptions{Window: time.Hour, MaxEntries: 2, Now: func() time.Time { return now }})

	// The table is filled by TWO agents, deliberately. Since
	// IDEM-11-FU-FAIRSHARE a SOLE agent's ceiling is its fair share
	// (maxEntries/2), so one agent can no longer reach the BUS-WIDE cap at all —
	// that is the documented price of the fair share (retention.go), not a
	// change to what this test is about. The subject here is still the bus-wide
	// cap: it fails closed and evicts nothing.
	for _, r := range []idem.Record{
		mustRecord(t, "k1", fp(1), base),
		agentRecord("bus1.bob-1", "k2", 2, base),
	} {
		if err := s.Remember(r); err != nil {
			t.Fatalf("Remember %s: %v", r.Key, err)
		}
	}
	if !s.Full() {
		t.Fatal("Full() = false at the cap, want true")
	}
	err := s.Remember(mustRecord(t, "k3", fp(3), base))
	if !errors.Is(err, idem.ErrCapacity) {
		t.Fatalf("Remember at the cap: err = %v, want ErrCapacity", err)
	}
	// And it is the BUS-WIDE cap specifically. A per-agent fair-share refusal
	// deliberately satisfies ErrCapacity too, so the assertion above alone would
	// still pass if the two checks inside Remember were ordered the wrong way
	// round and the share fired first — which would make this test silently
	// stop covering the subject named in its own title.
	if errors.Is(err, idem.ErrAgentQuota) {
		t.Fatalf("Remember at the cap: err = %v also satisfies ErrAgentQuota, so the refusal came from the per-agent share and not from the bus-wide cap this test is about", err)
	}
	// Nothing was evicted to make room.
	sc, scErr := idem.NewAgentScope(testAgent, idem.OpSend, "k1")
	if scErr != nil {
		t.Fatalf("NewAgentScope: %v", scErr)
	}
	if _, out := s.Lookup(sc, fp(1)); out != idem.OutcomeRetry {
		t.Fatalf("a live key was evicted to make room: outcome %v", out)
	}

	// Past the window there is room again, and Full() reflects that because it
	// expires first.
	now = base.Add(2 * time.Hour)
	if s.Full() {
		t.Fatal("Full() = true when every record has expired, want false")
	}
	if err := s.Remember(mustRecord(t, "k3", fp(3), now)); err != nil {
		t.Fatalf("Remember after expiry: %v", err)
	}
}

// TestStoreRejectsInvalidRecord proves Remember validates, so an unstorable
// record can never enter the table by the live path either.
func TestStoreRejectsInvalidRecord(t *testing.T) {
	s := idem.NewStore(idem.StoreOptions{})
	if err := s.Remember(idem.Record{Op: idem.OpSend, Key: "k"}); !errors.Is(err, idem.ErrInvalidRecord) {
		t.Fatalf("Remember of a record with no agent: err = %v, want ErrInvalidRecord", err)
	}
	if got := s.Stats().Count; got != 0 {
		t.Fatalf("a rejected record entered the table: Count = %d", got)
	}
}

// TestStoreDefaults proves the zero StoreOptions is the derived configuration,
// not an accidental zero window that would expire everything instantly.
func TestStoreDefaults(t *testing.T) {
	st := idem.NewStore(idem.StoreOptions{}).Stats()
	if st.Window != idem.RetentionWindow {
		t.Fatalf("default Window = %v, want RetentionWindow %v", st.Window, idem.RetentionWindow)
	}
	if st.MaxEntries != idem.MaxEntries {
		t.Fatalf("default MaxEntries = %d, want %d", st.MaxEntries, idem.MaxEntries)
	}
}

// TestStoreConcurrentAccess is the race-detector's target: Stats is read off
// the hub's write lock by CORE-5's inspect endpoint, so it must be safe
// alongside concurrent Lookup/Remember.
func TestStoreConcurrentAccess(t *testing.T) {
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	s := idem.NewStore(idem.StoreOptions{Now: func() time.Time { return base }})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = s.Stats()
			_ = s.Full()
			s.Expire()
		}
	}()
	for i := 0; i < 200; i++ {
		key := "k" + string(rune('a'+i%26))
		_ = s.Remember(mustRecord(t, key, fp(byte(i)), base))
		sc, err := idem.NewAgentScope(testAgent, idem.OpSend, key)
		if err != nil {
			t.Fatalf("NewAgentScope: %v", err)
		}
		s.Lookup(sc, fp(byte(i)))
	}
	<-done
}
