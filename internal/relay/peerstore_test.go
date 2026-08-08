package relay

// RELAY-10's evidence. The claim under test is a DURABILITY claim — "federation
// configuration survives a restart" — so the headline test is a real kill -9,
// not a polite Close: see TestPeerStoreSurvivesReplay.
//
// Everything else here pins a property that makes the headline safe rather than
// merely true once. Two of them are REGRESSION tests for defects the security
// gate found in the first version of this file, and they are named so:
// TestPeerStoreConfigSeqNeverRewinds and TestPeerStoreReplayIsClockIndependent.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	psLocalBus  = "bus-ps-local"
	psRemoteBus = "bus-ps-remote"
	// psOriginBus is the NON-ADJACENT bus in the laptop <-> internet <-> here
	// line: we pin its signing key and have no route to it whatsoever.
	psOriginBus = "bus-ps-origin"

	psURLGen1 = "https://peer-a.internal:8443"
	psURLGen2 = "https://peer-b.internal:9443"
)

// psKey returns a DETERMINISTIC Ed25519 public key from a one-byte seed, so the
// parent and the re-exec'd child agree on the fixture byte for byte without
// passing key material through the environment.
//
// crypto/ed25519's own key derivation; nothing here invents a construction
// (invariant 9). The PRIVATE half is discarded — only the public key is ever a
// pin.
func psKey(seed byte) ed25519.PublicKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
}

func psRoute(busID string, seq uint64, url string, at time.Time) PeerRecord {
	return PeerRecord{BusID: busID, ConfigSeq: seq, State: PeerRecordActive, BaseURL: url, UpdatedAt: at.UTC()}
}

func psTrust(busID string, seq uint64, at time.Time, keys ...ed25519.PublicKey) BusTrustRecord {
	return BusTrustRecord{BusID: busID, ConfigSeq: seq, State: PeerRecordActive, SigningKeys: keys, UpdatedAt: at.UTC()}
}

// psCommitted wraps a record in the wal.Committed shape Apply is handed. The
// record is ENCODED on the way through, so a test can never assert against a
// record the durable path would have refused.
func psCommitted(t *testing.T, rec busScopedRecord, prepare uint64) wal.Committed {
	t.Helper()
	body, canonical, err := canonicalPeerRecord(rec)
	if err != nil {
		t.Fatalf("encoding the fixture record for %s config_seq %d: %v", rec.recordBusID(), rec.recordSeq(), err)
	}
	kind := PeerRecordKind
	if _, isTrust := canonical.(BusTrustRecord); isTrust {
		kind = BusTrustRecordKind
	}
	return wal.Committed{PrepareIndex: prepare, CommitIndex: prepare + 1, Entry: wal.Entry{Kind: kind, Body: body}}
}

// psLogSink captures log output so a test can assert that a discard was LOGGED.
// Invariant 6 is explicit that SILENT discard is the defect, not discard itself,
// so "it was logged" is part of the contract and not decoration.
type psLogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *psLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *psLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// psStore builds a store with a captured logger and NO durable log (the
// replay-only shape). tune may adjust the options.
func psStore(t *testing.T, tune func(*PeerStoreOptions)) (*PeerStore, *psLogSink) {
	t.Helper()
	sink := &psLogSink{}
	o := PeerStoreOptions{BusID: psLocalBus, Logger: logging.New(sink, logging.LevelDebug)}
	if tune != nil {
		tune(&o)
	}
	st, err := NewPeerStore(o)
	if err != nil {
		t.Fatalf("NewPeerStore: %v", err)
	}
	return st, sink
}

// psLateLog defers binding the *wal.Log until after wal.Open has replayed into
// the store — the store must exist before the log that replays into it, and the
// log must exist before the store can write. invite's store_test uses the same
// indirection for the same reason.
type psLateLog struct{ l *wal.Log }

func (d *psLateLog) Write(e wal.Entry) (wal.Committed, error) { return d.l.Write(e) }

// psOpenStore opens a store over a REAL *wal.Log in dir, replaying whatever is
// already there. wrap, if non-nil, sits between the store and the log — the
// crash child uses it to inject the kill.
func psOpenStore(t *testing.T, dir string, tune func(*PeerStoreOptions), wrap func(PeerDurableLog) PeerDurableLog) (*PeerStore, *wal.Log) {
	t.Helper()
	d := &psLateLog{}
	var durable PeerDurableLog = d
	if wrap != nil {
		durable = wrap(d)
	}
	o := PeerStoreOptions{BusID: psLocalBus, Durable: durable}
	if tune != nil {
		tune(&o)
	}
	st, err := NewPeerStore(o)
	if err != nil {
		t.Fatalf("NewPeerStore: %v", err)
	}
	lg, err := wal.Open(wal.LogOptions{Dir: dir, Applier: st})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	d.l = lg
	return st, lg
}

func psAssertRoute(t *testing.T, ctx string, got, want PeerRecord) {
	t.Helper()
	if got.BusID != want.BusID || got.ConfigSeq != want.ConfigSeq || got.State != want.State ||
		got.BaseURL != want.BaseURL || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("%s:\n got  %+v\n want %+v", ctx, got, want)
	}
}

func psAssertTrust(t *testing.T, ctx string, got, want BusTrustRecord) {
	t.Helper()
	if got.BusID != want.BusID || got.ConfigSeq != want.ConfigSeq || got.State != want.State ||
		!sameKeySet(got.SigningKeys, want.SigningKeys) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("%s:\n got  %+v\n want %+v", ctx, got, want)
	}
}

// ---------------------------------------------------------------------------
// The records
// ---------------------------------------------------------------------------

// TestPeerRecordRoundTripsThroughAStrictDecoder proves the durable shape of both
// record kinds: what Encode writes is what Decode reads back, field for field,
// and the wire form is the documented one.
func TestPeerRecordRoundTripsThroughAStrictDecoder(t *testing.T) {
	at := time.Date(2026, 8, 8, 12, 0, 0, 123456789, time.UTC)

	routes := []PeerRecord{
		psRoute(psRemoteBus, 1, psURLGen1, at),
		{BusID: psRemoteBus, ConfigSeq: 7, State: PeerRecordRemoved, UpdatedAt: at},
	}
	for _, rec := range routes {
		body, err := rec.Encode()
		if err != nil {
			t.Fatalf("PeerRecord.Encode(%s): %v", rec.State, err)
		}
		got, err := DecodePeerRecord(body)
		if err != nil {
			t.Fatalf("DecodePeerRecord(%s): %v", body, err)
		}
		psAssertRoute(t, "round trip of a "+rec.State.String()+" route", got, rec)
		psAssertWireForm(t, body, rec.State)
		// THE ROUTING RECORD CARRIES NO KEY MATERIAL. If this ever fails, the
		// routing/trust split has been quietly undone and the non-adjacent
		// origin case (trust without a route) is unrepresentable again.
		if strings.Contains(string(body), "signing_key") {
			t.Errorf("the routing record carries key material: %s", body)
		}
	}

	trusts := []BusTrustRecord{
		psTrust(psOriginBus, 2, at, psKey(1)),
		psTrust(psOriginBus, 3, at, psKey(1), psKey(2)), // a rollover window
		{BusID: psOriginBus, ConfigSeq: 9, State: PeerRecordRemoved, UpdatedAt: at},
	}
	for _, rec := range trusts {
		body, err := rec.Encode()
		if err != nil {
			t.Fatalf("BusTrustRecord.Encode(%s): %v", rec.State, err)
		}
		got, err := DecodeBusTrustRecord(body)
		if err != nil {
			t.Fatalf("DecodeBusTrustRecord(%s): %v", body, err)
		}
		psAssertTrust(t, "round trip of a "+rec.State.String()+" trust record", got, rec)
		psAssertWireForm(t, body, rec.State)
		// THE TRUST RECORD CARRIES NO TRANSPORT — the other half of the split.
		if strings.Contains(string(body), "base_url") {
			t.Errorf("the trust record carries a base URL: %s", body)
		}
	}
}

// psAssertWireForm checks the conventions an operator reading the log with a
// pretty-printer depends on: a fixed state STRING (never the numeric enum) and
// an explicit schema version.
func psAssertWireForm(t *testing.T, body json.RawMessage, state PeerRecordState) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("the encoded record is not a JSON object: %v", err)
	}
	if string(raw["state"]) != `"`+state.String()+`"` {
		t.Errorf("state on disk is %s, want the fixed string %q", raw["state"], state.String())
	}
	if string(raw["v"]) != "1" {
		t.Errorf("v on disk is %s, want 1", raw["v"])
	}
	if _, ok := raw["bus_id"]; !ok {
		t.Errorf("the record carries no bus_id: %s", body)
	}
	if _, ok := raw["config_seq"]; !ok {
		t.Errorf("the record carries no config_seq: %s", body)
	}
	if _, ok := raw["rec"]; !ok {
		t.Errorf("the record carries no rec discriminator, so the two kinds' tombstones are byte-identical: %s", body)
	}
}

// TestPeerRouteDecoderRefusesDamage is the way-IN half of validate. A record off
// disk is UNTRUSTED INPUT even though this server wrote it, because "this server
// wrote it" is exactly the claim corruption disproves.
func TestPeerRouteDecoderRefusesDamage(t *testing.T) {
	good := `{"v":1,"rec":"peer","bus_id":"bus-ps-remote","config_seq":1,"state":"active","base_url":"https://peer-a.internal:8443","updated_at":"2026-08-08T12:00:00Z"}`
	if _, err := DecodePeerRecord([]byte(good)); err != nil {
		t.Fatalf("the control record must decode, or every case below is vacuous: %v", err)
	}
	cases := []struct{ name, body, want string }{
		{"an unknown field", strings.Replace(good, `"config_seq":1`, `"config_seq":1,"admin":true`, 1), "unknown field"},
		{"trailing data", good + `{"v":1}`, "trailing data"},
		{"a future version", strings.Replace(good, `"v":1`, `"v":2`, 1), "version 2"},
		{"an unknown state", strings.Replace(good, `"active"`, `"pending"`, 1), "not one of active, removed"},
		{"a zero config_seq", strings.Replace(good, `"config_seq":1`, `"config_seq":0`, 1), "config_seq is zero"},
		{"a config_seq past 2^53", strings.Replace(good, `"config_seq":1`, `"config_seq":9007199254740993`, 1), "exceeds"},
		{"a plaintext-http base URL", strings.Replace(good, "https://", "http://", 1), "invariant 11"},
		{"a base URL with a query", strings.Replace(good, ":8443", ":8443/?x=1", 1), "bare origin"},
		// A PATH is refused because the relay path is appended at every dial and
		// a stored path would ride along on all of them. peerURL, which every
		// live dial goes through, is MORE PERMISSIVE than this — the strictness
		// lives here, where the durable value is minted.
		{"a base URL with a path", strings.Replace(good, ":8443", ":8443/some/path", 1), "BARE ORIGIN"},
		{"a base URL with traversal in the path", strings.Replace(good, ":8443", ":8443/../../x", 1), "BARE ORIGIN"},
		{"a bus id carrying the qualification separator", strings.Replace(good, "bus-ps-remote", "bus.ps", 1), "bus id"},
		{"a tombstone still holding a base URL", `{"v":1,"rec":"peer","bus_id":"bus-ps-remote","config_seq":2,"state":"removed","base_url":"https://peer-a.internal:8443","updated_at":"2026-08-08T12:00:00Z"}`, "tombstone holds no live configuration"},
		// The two kinds' TOMBSTONES would be byte-identical without "rec", so a
		// Kind mix-up in future wiring would silently un-pin a bus.
		{"a TRUST tombstone read as a route", `{"v":1,"rec":"bustrust","bus_id":"bus-ps-remote","config_seq":2,"state":"removed","updated_at":"2026-08-08T12:00:00Z"}`, "the entry kind and the body disagree"},
		{"a base URL that is a bare origin with a forced empty query", strings.Replace(good, ":8443", ":8443?", 1), "bare origin"},
		{"a mangled timestamp", strings.Replace(good, "2026-08-08T12:00:00Z", "yesterday", 1), "RFC3339Nano"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodePeerRecord([]byte(tc.body))
			if err == nil {
				t.Fatalf("DecodePeerRecord accepted %s and returned %+v; a lenient decoder reinstates configuration nobody wrote", tc.name, got)
			}
			if !errors.Is(err, ErrInvalidPeerRecord) {
				t.Fatalf("err = %v, want it to wrap ErrInvalidPeerRecord", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q so an operator can act on it", err, tc.want)
			}
		})
	}
}

// TestBusTrustDecoderRefusesDamage is the same discipline on the record that
// carries the TRUST ANCHOR, where a lenient decoder reinstates key material
// nobody configured.
func TestBusTrustDecoderRefusesDamage(t *testing.T) {
	const k1 = "O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik="
	const k2 = "gTl3DqhrVfIaWQyzsWtWRj/ejtIWnvNYSC/M5NjrfxU="
	good := `{"v":1,"rec":"bustrust","bus_id":"bus-ps-origin","config_seq":1,"state":"active","bus_signing_keys":["` + k1 + `"],"updated_at":"2026-08-08T12:00:00Z"}`
	if _, err := DecodeBusTrustRecord([]byte(good)); err != nil {
		t.Fatalf("the control record must decode, or every case below is vacuous: %v", err)
	}
	// A two-key rollover window is LEGAL, and must stay legal: a scalar pin
	// would force an outage on every signing-key rotation.
	if _, err := DecodeBusTrustRecord([]byte(strings.Replace(good, `["`+k1+`"]`, `["`+k1+`","`+k2+`"]`, 1))); err != nil {
		t.Fatalf("a two-key rollover window was refused: %v", err)
	}
	cases := []struct{ name, body, want string }{
		{"an unknown field", strings.Replace(good, `"config_seq":1`, `"config_seq":1,"trusted":true`, 1), "unknown field"},
		{"trailing data", good + `[]`, "trailing data"},
		{"a future version", strings.Replace(good, `"v":1`, `"v":2`, 1), "version 2"},
		{"an active record pinning nothing", strings.Replace(good, `"bus_signing_keys":["`+k1+`"],`, "", 1), "at least one bus signing key"},
		{"a third key past the rollover pair", strings.Replace(good, `["`+k1+`"]`, `["`+k1+`","`+k2+`","`+k1+`"]`, 1), "at most 2"},
		{"a repeated key", strings.Replace(good, `["`+k1+`"]`, `["`+k1+`","`+k1+`"]`, 1), "a pin set is a set"},
		{"a short key", strings.Replace(good, k1, "AAAA", 1), "want exactly 32"},
		{"an all-zero key", strings.Replace(good, k1, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 1), "all zero"},
		{"a key that is not base64", strings.Replace(good, k1, "not!base64!!", 1), "not base64"},
		{"a tombstone still pinning a key", `{"v":1,"rec":"bustrust","bus_id":"bus-ps-origin","config_seq":2,"state":"removed","bus_signing_keys":["` + k1 + `"],"updated_at":"2026-08-08T12:00:00Z"}`, "tombstone holds no trust anchor"},
		{"a ROUTE tombstone read as a trust record", `{"v":1,"rec":"peer","bus_id":"bus-ps-origin","config_seq":2,"state":"removed","updated_at":"2026-08-08T12:00:00Z"}`, "the entry kind and the body disagree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeBusTrustRecord([]byte(tc.body))
			if err == nil {
				t.Fatalf("DecodeBusTrustRecord accepted %s and returned %+v", tc.name, got)
			}
			if !errors.Is(err, ErrInvalidPeerRecord) {
				t.Fatalf("err = %v, want it to wrap ErrInvalidPeerRecord", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The split: trust without a route, route without trust
// ---------------------------------------------------------------------------

// TestPeerStoreSeparatesTrustFromRouting is the shape requirement RELAY-7's
// deep-dive produced, and the reason the record is two records.
//
// In laptop(A) <-> internet(B) <-> this machine(C), C never peers with A but
// must pin A's bus signing key, because a message ORIGINATING at A is verified
// by C against that pin and B is not allowed to vouch for it. A single record
// coupling an address to a key cannot express that at all.
func TestPeerStoreSeparatesTrustFromRouting(t *testing.T) {
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg.Close() }()

	// B: the adjacent hop. A route, and no pins — we relay THROUGH it and
	// accept no origin traffic FROM it.
	if _, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1}); err != nil {
		t.Fatalf("Put(route to the hop): %v", err)
	}
	// A: the non-adjacent origin. Pins, and NO ROUTE AT ALL.
	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("PutTrust(the non-adjacent origin): %v", err)
	}

	if _, ok := st.Lookup(psOriginBus); ok {
		t.Errorf("the non-adjacent origin acquired a ROUTE; we have no address for it and must never invent one")
	}
	if got := st.PinnedKeys(psOriginBus); len(got) != 1 || !bytes.Equal(got[0], psKey(1)) {
		t.Fatalf("PinnedKeys(non-adjacent origin) = %x, want the single pinned key; without it a relayed message from that bus cannot be verified at all", got)
	}
	if got := st.PinnedKeys(psRemoteBus); got != nil {
		t.Errorf("the routing hop acquired PINS it was never given: %x", got)
	}
	if _, ok := st.Lookup(psRemoteBus); !ok {
		t.Errorf("the routing hop lost its route")
	}

	// Both survive a restart, independently.
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}
	st2, lg2 := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg2.Close() }()
	if got := st2.PinnedKeys(psOriginBus); len(got) != 1 || !bytes.Equal(got[0], psKey(1)) {
		t.Errorf("after a restart the non-adjacent origin's pin is %x, want the configured key", got)
	}
	if _, ok := st2.Lookup(psOriginBus); ok {
		t.Errorf("after a restart the non-adjacent origin has a route")
	}
	if rec, ok := st2.Lookup(psRemoteBus); !ok || rec.BaseURL != psURLGen1 {
		t.Errorf("after a restart the hop's route is %+v, want %s", rec, psURLGen1)
	}
	if len(st2.TrustedBuses()) != 1 || len(st2.ActivePeers()) != 1 {
		t.Errorf("after a restart: %d trusted buses and %d routes, want 1 and 1", len(st2.TrustedBuses()), len(st2.ActivePeers()))
	}
}

// ---------------------------------------------------------------------------
// The upsert
// ---------------------------------------------------------------------------

// TestPeerStoreUpsertIsMonotonicOnTheConfigSeq is the property the whole record
// shape exists to support. THE CASE THAT MATTERS is the stale one: an older
// record arriving after a newer one must not put a bus's PINNED SIGNING KEYS
// back to a previous set. Nothing downstream could tell that downgrade apart
// from a legitimate rotation.
func TestPeerStoreUpsertIsMonotonicOnTheConfigSeq(t *testing.T) {
	at1 := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	at2 := at1.Add(time.Hour)
	gen1 := psTrust(psOriginBus, 1, at1, psKey(1))
	gen2 := psTrust(psOriginBus, 2, at2, psKey(2))

	st, sink := psStore(t, nil)
	for _, rec := range []busScopedRecord{gen1, gen2} {
		if err := st.Apply(psCommitted(t, rec, 10+rec.recordSeq())); err != nil {
			t.Fatalf("Apply(config_seq %d): %v", rec.recordSeq(), err)
		}
	}
	got, ok := st.LookupTrust(psOriginBus)
	if !ok {
		t.Fatalf("after two records the bus is not in the trust table at all")
	}
	psAssertTrust(t, "after applying config_seq 1 then 2", got, gen2)

	// A re-applied IDENTICAL record is idempotent and SILENT: the same log
	// replayed twice, or a live fold Apply already performed.
	before := sink.String()
	if err := st.Apply(psCommitted(t, gen2, 12)); err != nil {
		t.Fatalf("Apply(gen2 again): %v", err)
	}
	if sink.String() != before {
		t.Errorf("re-applying the identical record logged something; an idempotent re-apply must be silent:\n%s", strings.TrimPrefix(sink.String(), before))
	}

	// THE DOWNGRADE. gen1 pins an older key; applying it after gen2 must change
	// nothing, and must be LOUD.
	if err := st.Apply(psCommitted(t, gen1, 30)); err != nil {
		t.Fatalf("Apply(stale gen1) returned %v; Apply must never return an error", err)
	}
	got, _ = st.LookupTrust(psOriginBus)
	psAssertTrust(t, "after a STALE config_seq 1 record arrived behind 2", got, gen2)
	if out := sink.String(); !strings.Contains(out, "NON-MONOTONIC") || !strings.Contains(out, "level=error") {
		t.Errorf("the stale record was discarded but not logged loudly at ERROR; silent discard is the defect, not discard itself. Log was:\n%s", out)
	}

	// An EQUAL sequence carrying a DIFFERENT record: refused, first wins. This
	// can only arise from damage, and between "keep what is applied" and "accept
	// an unexpected change to a pinned key", only the first is safe.
	if err := st.Apply(psCommitted(t, psTrust(psOriginBus, 2, at2, psKey(9)), 40)); err != nil {
		t.Fatalf("Apply(conflict) returned %v; Apply must never return an error", err)
	}
	got, _ = st.LookupTrust(psOriginBus)
	psAssertTrust(t, "after a DIFFERENT record claimed the applied config_seq", got, gen2)
}

// TestPeerStoreReplayEnforcesTheCapacityCap pins the rule that a cap applied
// only on the write path is not a cap. MaxPeers is a MEMORY bound, and replay is
// a path that can insert.
func TestPeerStoreReplayEnforcesTheCapacityCap(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	st, sink := psStore(t, func(o *PeerStoreOptions) { o.MaxPeers = 2 })

	for i, bus := range []string{"bus-ps-one", "bus-ps-two", "bus-ps-three"} {
		if err := st.Apply(psCommitted(t, psRoute(bus, uint64(i+1), psURLGen1, at), uint64(10*i))); err != nil {
			t.Fatalf("Apply(%s) returned %v; Apply must never return an error", bus, err)
		}
		if err := st.Apply(psCommitted(t, psTrust(bus, uint64(i+10), at, psKey(byte(i+1))), uint64(100+10*i))); err != nil {
			t.Fatalf("Apply(trust %s) returned %v", bus, err)
		}
	}
	if got := len(st.ActivePeers()); got != 2 {
		t.Fatalf("the route table holds %d entries after replaying 3 records against a cap of 2; a bound one path can exceed is not a bound", got)
	}
	if got := len(st.TrustedBuses()); got != 2 {
		t.Fatalf("the trust table holds %d entries against a cap of 2", got)
	}
	if _, ok := st.Lookup("bus-ps-three"); ok {
		t.Errorf("the third route was admitted past the cap on the REPLAY path")
	}
	if out := sink.String(); !strings.Contains(out, "level=error") || !strings.Contains(out, "bus-ps-three") {
		t.Errorf("the capacity discard was not logged loudly and specifically. Log was:\n%s", out)
	}
}

// TestPeerStoreRefusesARecordNamingOurOwnBus: a self-peer is a routing loop and
// a namespace collision at once, and the check has to be on the REPLAY path too
// — a record naming us would otherwise be installed by recovery with nobody
// having asked for it.
func TestPeerStoreRefusesARecordNamingOurOwnBus(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	st, sink := psStore(t, nil)
	for _, spelling := range []string{psLocalBus, strings.ToUpper(psLocalBus)} {
		if err := st.Apply(psCommitted(t, psRoute(spelling, 1, psURLGen1, at), 10)); err != nil {
			t.Fatalf("Apply returned %v; Apply must never return an error", err)
		}
		if err := st.Apply(psCommitted(t, psTrust(spelling, 2, at, psKey(1)), 12)); err != nil {
			t.Fatalf("Apply(trust) returned %v", err)
		}
		if _, ok := st.Lookup(spelling); ok {
			t.Fatalf("a route naming our own bus (%q) was installed by replay", spelling)
		}
		if got := st.PinnedKeys(spelling); got != nil {
			t.Fatalf("a trust record naming our own bus (%q) was installed by replay", spelling)
		}
	}
	if out := sink.String(); !strings.Contains(out, "level=error") {
		t.Errorf("the self-peer records were discarded without an ERROR line. Log was:\n%s", out)
	}
}

// TestPeerStoreApplyNeverReturnsAnError pins invariant 6's consequence: a
// non-nil error from Apply poisons a live log (wal.ErrDiverged) or refuses a
// start. Recovery ALWAYS reaches a running server, so every failure here is a
// logged discard instead.
func TestPeerStoreApplyNeverReturnsAnError(t *testing.T) {
	st, sink := psStore(t, nil)
	cases := []struct {
		name  string
		entry wal.Entry
		quiet bool
	}{
		{"another package's record kind", wal.Entry{Kind: "message", Body: json.RawMessage(`{"anything":true}`)}, true},
		{"a body that is not a route record", wal.Entry{Kind: PeerRecordKind, Body: json.RawMessage(`{"nope":1}`)}, false},
		{"a body that is not a trust record", wal.Entry{Kind: BusTrustRecordKind, Body: json.RawMessage(`{"nope":1}`)}, false},
		{"a body that is not JSON at all", wal.Entry{Kind: PeerRecordKind, Body: json.RawMessage(`not json`)}, false},
		{"an empty body", wal.Entry{Kind: BusTrustRecordKind}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := sink.String()
			if err := st.Apply(wal.Committed{PrepareIndex: 1, CommitIndex: 2, Entry: tc.entry}); err != nil {
				t.Fatalf("Apply returned %v, want nil: a non-nil error poisons a live log and refuses a start", err)
			}
			added := strings.TrimPrefix(sink.String(), before)
			if tc.quiet && added != "" {
				t.Errorf("an entry belonging to another package was treated as damage:\n%s", added)
			}
			if !tc.quiet && !strings.Contains(added, "level=error") {
				t.Errorf("the discard was SILENT, which is the actual defect (rated P0). Log was:\n%s", added)
			}
		})
	}
	if len(st.ActivePeers()) != 0 || len(st.TrustedBuses()) != 0 {
		t.Errorf("a record entered a table from a damaged entry")
	}
}

// ---------------------------------------------------------------------------
// REGRESSION: the two defects the security gate found in the first version
// ---------------------------------------------------------------------------

// TestPeerStoreConfigSeqNeverRewinds is the regression test for the P0.
//
// The first version of this file derived the next sequence number from THE
// PEER'S OWN ENTRY. That entry can legitimately leave the table — swept once its
// tombstone expires, or discarded on replay by the capacity cap — after which
// the next write restarted at 1 while the durable log still held records at
// 1..N. On the following replay the OLD generation, arriving first at an equal
// sequence, WON, and the operator's current configuration was silently replaced
// by a superseded one.
//
// The fix is a bus-wide high-water mark raised by EVERY record replay decodes,
// BEFORE any decision to discard it, and never lowered by a sweep or a discard.
// Both routes to the old defect are exercised here.
func TestPeerStoreConfigSeqNeverRewinds(t *testing.T) {
	t.Run("after a tombstone is swept", func(t *testing.T) {
		dir := t.TempDir()
		base := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
		now := base
		st, lg := psOpenStore(t, dir,
			func(o *PeerStoreOptions) {
				o.TombstoneRetention = time.Hour
				o.Now = func() time.Time { return now }
			}, nil)
		defer func() { _ = lg.Close() }()

		first, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, err := st.Remove(psRemoteBus); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		// Past the retention: the tombstone, and with it the only per-peer
		// record of how far that peer's sequence had got, is swept.
		now = base.Add(2 * time.Hour)
		if _, ok := st.Lookup(psRemoteBus); ok {
			t.Fatalf("the tombstone outlived its retention; this sub-test needs it gone")
		}
		again, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen2})
		if err != nil {
			t.Fatalf("the re-peer after the sweep: %v", err)
		}
		if again.ConfigSeq <= first.ConfigSeq {
			t.Fatalf("the re-peer was written at config_seq %d, at or below the swept peer's %d: the sequence REWOUND, and the durable log now holds two different records claiming one number", again.ConfigSeq, first.ConfigSeq)
		}

		// And prove the consequence directly: with the REMOVAL record lost —
		// which invariant 6 explicitly licenses recovery to do — a fresh store
		// replaying only the first and last records must serve the LATEST
		// address, not the superseded one.
		committed := psReplayCommitted(t, dir)
		if len(committed) != 3 {
			t.Fatalf("the log holds %d committed entries, want 3 (put, remove, re-put)", len(committed))
		}
		fresh, _ := psStore(t, nil)
		for _, c := range []wal.Committed{committed[0], committed[2]} {
			if err := fresh.Apply(c); err != nil {
				t.Fatalf("Apply: %v", err)
			}
		}
		got, ok := fresh.Lookup(psRemoteBus)
		if !ok {
			t.Fatalf("the peer was not recovered at all")
		}
		if got.BaseURL != psURLGen2 {
			t.Fatalf("after replay the peer's address is %q, want the operator's CURRENT %q: a superseded generation won", got.BaseURL, psURLGen2)
		}
	})

	t.Run("after a capacity discard", func(t *testing.T) {
		dir := t.TempDir()
		at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
		st, lg := psOpenStore(t, dir, func(o *PeerStoreOptions) { o.MaxPeers = 1 }, nil)
		defer func() { _ = lg.Close() }()

		// One slot, two records: the second is DISCARDED — and must still raise
		// the high-water mark, or its number is handed out again.
		if err := st.Apply(psCommitted(t, psRoute(psRemoteBus, 5, psURLGen1, at), 10)); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if err := st.Apply(psCommitted(t, psRoute("bus-ps-crowded", 6, psURLGen1, at), 12)); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if _, ok := st.Lookup("bus-ps-crowded"); ok {
			t.Fatalf("the second record was admitted; this sub-test needs the cap to discard it")
		}
		rec, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen2})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if rec.ConfigSeq <= 6 {
			t.Fatalf("the next write took config_seq %d, at or below the DISCARDED record's 6; invariant 1 is explicit that recovery may not reissue a number it has handed out, EVEN FOR A RECORD IT DISCARDS", rec.ConfigSeq)
		}
	})
}

// TestPeerStoreASweptTombstoneStillRefusesAnOlderRecord is the regression test
// for the hole BOTH gates found in the second version of this file.
//
// A tombstone does two jobs, and the sweep only ends one of them. While it is
// present, an older duplicate is refused by the ordinary monotonicity check.
// Once it is swept the bus is UNKNOWN again, and the insert branch would take
// whatever arrives — so a duplicated or reordered older record put a withdrawn
// route back at its old address and, far worse, put a REVOKED PINNED SIGNING KEY
// back. The file documented `PeerStore.configSeq` as covering this; it did not,
// because that counter is a MINTING floor that the admission path never read.
//
// `busTable.sweptMax` is the admission floor that actually covers it, and this
// test is the thing that would go red if it were removed.
func TestPeerStoreASweptTombstoneStillRefusesAnOlderRecord(t *testing.T) {
	base := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	var now time.Time
	st, sink := psStore(t, func(o *PeerStoreOptions) {
		o.TombstoneRetention = time.Hour
		o.Now = func() time.Time { return now }
	})
	now = base

	// Pin a key, then REVOKE it. Same for a route.
	pinned := psTrust(psOriginBus, 1, base, psKey(1))
	routed := psRoute(psRemoteBus, 2, psURLGen1, base)
	for i, rec := range []busScopedRecord{
		pinned,
		routed,
		BusTrustRecord{BusID: psOriginBus, ConfigSeq: 3, State: PeerRecordRemoved, UpdatedAt: base},
		PeerRecord{BusID: psRemoteBus, ConfigSeq: 4, State: PeerRecordRemoved, UpdatedAt: base},
	} {
		if err := st.Apply(psCommitted(t, rec, uint64(10*i))); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}

	// Past the retention: both tombstones are swept, and both buses are unknown
	// again. This is the state in which the old code was exposed.
	now = base.Add(2 * time.Hour)
	if _, ok := st.LookupTrust(psOriginBus); ok {
		t.Fatalf("the trust tombstone outlived its retention; this test needs it gone")
	}
	if _, ok := st.Lookup(psRemoteBus); ok {
		t.Fatalf("the route tombstone outlived its retention; this test needs it gone")
	}

	// The duplicates recovery is allowed to produce.
	before := sink.String()
	for i, rec := range []busScopedRecord{pinned, routed} {
		if err := st.Apply(psCommitted(t, rec, uint64(100+10*i))); err != nil {
			t.Fatalf("Apply(duplicate) returned %v; Apply must never return an error", err)
		}
	}
	if got := st.PinnedKeys(psOriginBus); got != nil {
		t.Fatalf("a duplicated older record RESURRECTED a revoked pinned bus signing key (%x); the operator revoked that anchor and nothing downstream would ever know it was back", got)
	}
	if got, ok := st.Lookup(psRemoteBus); ok {
		t.Fatalf("a duplicated older record resurrected a withdrawn route: %+v", got)
	}
	added := strings.TrimPrefix(sink.String(), before)
	if !strings.Contains(added, "level=error") || !strings.Contains(added, "RESURRECT") {
		t.Errorf("the resurrection attempt was refused but not logged loudly and specifically. Log was:\n%s", added)
	}

	// The floor must NOT block the operator re-configuring either bus: a live
	// write mints a sequence above everything ever applied.
	if err := st.Apply(psCommitted(t, psTrust(psOriginBus, 9, now, psKey(2)), 200)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := st.PinnedKeys(psOriginBus); len(got) != 1 || !bytes.Equal(got[0], psKey(2)) {
		t.Fatalf("a FRESH pin at a higher sequence was refused after the sweep (%x); the floor must stop resurrection, not re-configuration", got)
	}
}

// TestPeerStoreSweepsATombstoneStampedInTheFuture pins the symmetric half of the
// retention predicate.
//
// A record stamped ahead of the clock has a NEGATIVE age, so an "older than
// retention" test never fires and the tombstone is unsweepable forever. That is
// reachable without touching the log: write() stamps the local clock, so an
// operator machine that is far ahead when a peer is withdrawn writes one. Enough
// of them fills the bounded table and every new peering is refused. Dropping it
// is safe because its sequence goes to the admission floor first — which the
// second half of this test checks, since a sweep that forgot that would be
// trading a capacity leak for a resurrection.
func TestPeerStoreSweepsATombstoneStampedInTheFuture(t *testing.T) {
	base := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	now := base
	st, _ := psStore(t, func(o *PeerStoreOptions) {
		o.TombstoneRetention = time.Hour
		o.Now = func() time.Time { return now }
	})
	stale := psTrust(psOriginBus, 1, base, psKey(1))
	if err := st.Apply(psCommitted(t, stale, 10)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Withdrawn by a machine whose clock is a year ahead.
	if err := st.Apply(psCommitted(t, BusTrustRecord{BusID: psOriginBus, ConfigSeq: 2, State: PeerRecordRemoved, UpdatedAt: base.AddDate(1, 0, 0)}, 12)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := st.LookupTrust(psOriginBus); ok {
		t.Fatalf("a tombstone stamped a YEAR ahead of the clock was retained; nothing will ever sweep it and it holds a slot in a 64-entry table forever")
	}
	// AND AT THE SATURATION BOUNDARY. time.Duration saturates at +/-292 years and
	// negating it there overflows back to itself, so a symmetric test written on
	// durations silently stops firing exactly where the stamp is most absurd.
	if err := st.Apply(psCommitted(t, psTrust("bus-ps-far", 3, base, psKey(2)), 16)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := st.Apply(psCommitted(t, BusTrustRecord{BusID: "bus-ps-far", ConfigSeq: 4, State: PeerRecordRemoved, UpdatedAt: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)}, 18)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := st.LookupTrust("bus-ps-far"); ok {
		t.Fatalf("a tombstone stamped in the year 9999 was retained: the sweep saturated its own arithmetic and stopped firing")
	}
	if got := st.PinnedKeys(psOriginBus); got != nil {
		t.Fatalf("sweeping the future-stamped tombstone left the bus PINNED: %x", got)
	}
	// And the sweep must have carried the sequence to the floor, or it has just
	// traded a capacity leak for a resurrection.
	if err := st.Apply(psCommitted(t, stale, 14)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := st.PinnedKeys(psOriginBus); got != nil {
		t.Fatalf("after the future-stamped tombstone was swept, the pre-revocation pin came back: %x", got)
	}
}

// TestPeerStoreReplayIsClockIndependent is the regression test for the P1.
//
// The tombstone sweep runs during replay, so the worry is that a skewed clock
// makes recovery non-deterministic — a real trigger (NTP step-back, a restored
// VM snapshot, a container with a skewed clock). It does not, because the
// SEQUENCE NUMBER and not the tombstone decides which generation wins. This test
// replays one identical history under two clocks a decade apart and asserts the
// recovered state is the same.
func TestPeerStoreReplayIsClockIndependent(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	history := []wal.Committed{}
	for i, rec := range []busScopedRecord{
		psRoute(psRemoteBus, 1, psURLGen1, at),
		PeerRecord{BusID: psRemoteBus, ConfigSeq: 2, State: PeerRecordRemoved, UpdatedAt: at.Add(time.Minute)},
		psRoute(psRemoteBus, 3, psURLGen2, at.Add(2*time.Minute)),
		psTrust(psOriginBus, 4, at.Add(3*time.Minute), psKey(1)),
		BusTrustRecord{BusID: psOriginBus, ConfigSeq: 5, State: PeerRecordRemoved, UpdatedAt: at.Add(4 * time.Minute)},
		// THE CASES THAT MAKE THIS TEST WORTH RUNNING: older records replayed
		// again behind a withdrawal. Under a clock past the retention the
		// tombstones are swept mid-replay, so ONLY the admission floor stands
		// between these and a resurrected route / a resurrected REVOKED PIN.
		psRoute(psRemoteBus, 1, psURLGen1, at),
		psTrust(psOriginBus, 4, at.Add(3*time.Minute), psKey(1)),
	} {
		history = append(history, psCommitted(t, rec, uint64(10*i)))
	}

	replayUnder := func(now time.Time) (PeerRecord, BusTrustRecord, []ed25519.PublicKey) {
		st, _ := psStore(t, func(o *PeerStoreOptions) {
			o.TombstoneRetention = time.Hour
			o.Now = func() time.Time { return now }
		})
		for _, c := range history {
			if err := st.Apply(c); err != nil {
				t.Fatalf("Apply: %v", err)
			}
		}
		route, _ := st.Lookup(psRemoteBus)
		trust, _ := st.LookupTrust(psOriginBus)
		return route, trust, st.PinnedKeys(psOriginBus)
	}

	// A decade in the future: every tombstone is swept mid-replay. A decade in
	// the past: the tombstones are stamped a decade AHEAD of that clock, which
	// the symmetric sweep also drops — so both clocks exercise the floor.
	futureRoute, futureTrust, futurePins := replayUnder(at.AddDate(10, 0, 0))
	pastRoute, pastTrust, pastPins := replayUnder(at.AddDate(-10, 0, 0))

	psAssertRoute(t, "the route recovered under a clock a decade AHEAD", futureRoute, pastRoute)
	psAssertTrust(t, "the trust recovered under a clock a decade AHEAD", futureTrust, pastTrust)
	if futureRoute.BaseURL != psURLGen2 || futureRoute.ConfigSeq != 3 {
		t.Fatalf("recovery produced %+v, want the last generation (config_seq 3, %s) whatever the clock says", futureRoute, psURLGen2)
	}
	// The trust record was WITHDRAWN at config_seq 5 and its old generation
	// replayed again afterwards. Whatever the clock did to the tombstone, the
	// revoked pin must not be back.
	for _, pins := range [][]ed25519.PublicKey{futurePins, pastPins} {
		if len(pins) != 0 {
			t.Fatalf("a REVOKED pinned bus signing key came back as %x; the withdrawal was replayed before the duplicate that resurrected it", pins)
		}
	}
}

// ---------------------------------------------------------------------------
// The live path
// ---------------------------------------------------------------------------

// TestPeerStoreWritesAreDurableAndIdempotent exercises the write path against a
// real *wal.Log: a write lands, an identical write does nothing, a rotation
// advances the sequence, and a removal leaves a tombstone.
func TestPeerStoreWritesAreDurableAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg.Close() }()

	rec, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if rec.ConfigSeq != 1 || rec.State != PeerRecordActive {
		t.Fatalf("the first Put produced config_seq %d / state %s, want 1 / active", rec.ConfigSeq, rec.State)
	}

	// An identical write is a NO-OP: an operator's config run that re-applies
	// the same peering must not append a record on every pass.
	if again, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1}); err != nil {
		t.Fatalf("the identical Put: %v", err)
	} else if again.ConfigSeq != 1 {
		t.Errorf("an identical Put advanced the sequence to %d; nothing changed, so nothing should have been written", again.ConfigSeq)
	}
	trust1, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}})
	if err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	if again, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("the identical PutTrust: %v", err)
	} else if again.ConfigSeq != trust1.ConfigSeq {
		t.Errorf("an identical PutTrust advanced the sequence to %d, want %d", again.ConfigSeq, trust1.ConfigSeq)
	}
	// A ROLLOVER: two keys pinned at once, then the old one dropped.
	roll, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1), psKey(2)}})
	if err != nil {
		t.Fatalf("PutTrust(rollover): %v", err)
	}
	if len(roll.SigningKeys) != 2 || roll.ConfigSeq <= trust1.ConfigSeq {
		t.Fatalf("the rollover produced %+v, want two keys at a higher sequence", roll)
	}

	// A caller that mutates the slice it handed us must not be able to reach the
	// stored trust anchor.
	handed := []ed25519.PublicKey{psKey(3)}
	if _, err := st.PutTrust(BusTrust{BusID: psRemoteBus, SigningKeys: handed}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	copy(handed[0], make([]byte, ed25519.PublicKeySize))
	if got := st.PinnedKeys(psRemoteBus); len(got) != 1 || !bytes.Equal(got[0], psKey(3)) {
		t.Fatalf("mutating the caller's slice changed the STORED pin to %x; a trust anchor must not be reachable without going through the write path", got)
	}

	// AND ON THE WAY OUT. This is the direction that has no other defence: a
	// record written through PutTrust is re-decoded before it is stored, so its
	// key slices are freshly allocated whatever the write path does — but a READ
	// hands the caller slices, and if those aliased the stored record then any
	// caller of PinnedKeys could rewrite this bus's trust anchor by assigning to
	// a slice it was given. Replacing copySigningKeys with `return in` passes
	// every other test in this package; this is the one that catches it.
	handedBack := st.PinnedKeys(psRemoteBus)
	if len(handedBack) != 1 {
		t.Fatalf("PinnedKeys returned %d keys, want 1", len(handedBack))
	}
	copy(handedBack[0], bytes.Repeat([]byte{0xAA}, ed25519.PublicKeySize))
	if got := st.PinnedKeys(psRemoteBus); !bytes.Equal(got[0], psKey(3)) {
		t.Fatalf("mutating the slice PinnedKeys RETURNED changed the stored pin to %x; a trust anchor must not be reachable through a read", got[0])
	}
	if rec, _ := st.LookupTrust(psRemoteBus); !bytes.Equal(rec.SigningKeys[0], psKey(3)) {
		t.Fatalf("the same through LookupTrust: the stored pin is %x", rec.SigningKeys[0])
	}

	tomb, err := st.Remove(psRemoteBus)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if tomb.State != PeerRecordRemoved || tomb.BaseURL != "" {
		t.Fatalf("Remove produced %+v, want a removed record holding no live configuration", tomb)
	}
	if _, err := st.Remove(psRemoteBus); err != nil {
		t.Errorf("removing an already-removed route gave %v, want a silent no-op", err)
	}
	if _, err := st.Remove("bus-ps-never"); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("removing an unknown bus gave %v, want ErrUnknownPeer", err)
	}
	if _, err := st.RemoveTrust("bus-ps-never"); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("un-pinning an unknown bus gave %v, want ErrUnknownPeer", err)
	}

	// A re-peer AFTER a removal is legitimate and must work — which is why
	// monotonicity is keyed on the sequence and not on the lifecycle state.
	repeered, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1})
	if err != nil {
		t.Fatalf("re-peering a removed bus: %v", err)
	}
	if repeered.State != PeerRecordActive || repeered.ConfigSeq <= tomb.ConfigSeq {
		t.Errorf("the re-peer produced %+v, want an active record above config_seq %d", repeered, tomb.ConfigSeq)
	}

	// RemoveTrust is the revocation path: after it, nothing is pinned.
	if _, err := st.RemoveTrust(psOriginBus); err != nil {
		t.Fatalf("RemoveTrust: %v", err)
	}
	if got := st.PinnedKeys(psOriginBus); got != nil {
		t.Errorf("PinnedKeys after revocation = %x, want nothing; an unknown bus and a revoked one must answer the same", got)
	}
}

// TestPeerStoreRefusesToWriteWithoutADurableLog: configuration forgotten on
// restart is a trust anchor that silently stops existing, so the answer is a
// refusal rather than a degraded in-memory mode.
func TestPeerStoreRefusesToWriteWithoutADurableLog(t *testing.T) {
	st, _ := psStore(t, nil)
	if _, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1}); !errors.Is(err, ErrPeerNotDurable) {
		t.Errorf("Put without a durable log gave %v, want ErrPeerNotDurable", err)
	}
	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); !errors.Is(err, ErrPeerNotDurable) {
		t.Errorf("PutTrust without a durable log gave %v, want ErrPeerNotDurable", err)
	}
	if _, err := st.Remove(psRemoteBus); !errors.Is(err, ErrPeerNotDurable) {
		t.Errorf("Remove without a durable log gave %v, want ErrPeerNotDurable", err)
	}
}

// TestPeerStoreConcurrentWritesRespectTheCapAndTheSequence drives PeerStore.writeMu,
// which the rest of the suite only reaches one write at a time. Under -race it is
// also the data-race check on the store's own fields.
//
// THREE properties must survive concurrency: the cap is never exceeded, no two
// durable records share a sequence number, and — the one that is easy to miss and
// that cost this file a review round — the sequences in the LOG are strictly
// increasing, i.e. WAL order is mint order. Everything busTable.sweptMax does
// rests on that last one.
func TestPeerStoreConcurrentWritesRespectTheCapAndTheSequence(t *testing.T) {
	dir := t.TempDir()
	const cap0 = 4
	st, lg := psOpenStore(t, dir, func(o *PeerStoreOptions) { o.MaxPeers = cap0 }, nil)
	defer func() { _ = lg.Close() }()

	var (
		wg        sync.WaitGroup
		succeeded int64
	)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bus := fmt.Sprintf("bus-ps-c%d", i%6)
			if _, err := st.Put(PeerConfig{BusID: bus, BaseURL: psURLGen1}); err == nil {
				atomic.AddInt64(&succeeded, 1)
			}
			if _, err := st.PutTrust(BusTrust{BusID: bus, SigningKeys: []ed25519.PublicKey{psKey(byte(i%6 + 1))}}); err == nil {
				atomic.AddInt64(&succeeded, 1)
			}
		}(i)
	}
	wg.Wait()

	// WITHOUT THIS the test passes against a store that refused every write:
	// zero records satisfies "at most cap" and "no duplicate sequence" perfectly.
	if got := atomic.LoadInt64(&succeeded); got == 0 {
		t.Fatalf("every concurrent write was refused; the assertions below would then hold vacuously")
	}
	if len(st.ActivePeers()) == 0 || len(st.TrustedBuses()) == 0 {
		t.Fatalf("no record survived the concurrent writes: %d routes, %d trusted buses", len(st.ActivePeers()), len(st.TrustedBuses()))
	}

	if got := len(st.ActivePeers()); got > cap0 {
		t.Fatalf("the route table holds %d entries against a cap of %d", got, cap0)
	}
	if got := len(st.TrustedBuses()); got > cap0 {
		t.Fatalf("the trust table holds %d entries against a cap of %d", got, cap0)
	}
	// AND THE ORDER. This is the property busTable.sweptMax rests on: if the log
	// can hold a lower sequence AFTER a higher one, a legitimate acknowledged
	// write is refused by the floor on a later replay and is lost for good. Both
	// gates reproduced exactly that against the unserialised version of write().
	var lastSeq uint64
	seen := map[uint64]string{}
	for _, c := range psReplayCommitted(t, dir) {
		var (
			busID string
			seq   uint64
		)
		switch c.Entry.Kind {
		case PeerRecordKind:
			r, err := DecodePeerRecord(c.Entry.Body)
			if err != nil {
				t.Fatalf("a committed route record does not decode: %v", err)
			}
			busID, seq = r.BusID, r.ConfigSeq
		case BusTrustRecordKind:
			r, err := DecodeBusTrustRecord(c.Entry.Body)
			if err != nil {
				t.Fatalf("a committed trust record does not decode: %v", err)
			}
			busID, seq = r.BusID, r.ConfigSeq
		default:
			t.Fatalf("unexpected entry kind %q", c.Entry.Kind)
		}
		if prev, dup := seen[seq]; dup {
			t.Fatalf("two durable records share config_seq %d (%s and %s); the monotonic upsert cannot order them and one silently wins", seq, prev, busID)
		}
		if seq <= lastSeq {
			t.Fatalf("config_seq %d (%s) appears in the log AFTER %d: the order of records in the log is not the order their sequences were minted in, so a later replay past the tombstone retention will refuse this record and permanently lose an acknowledged write", seq, busID, lastSeq)
		}
		lastSeq = seq
		seen[seq] = busID
	}
}

// TestPeerStoreTombstoneRetentionIsAPurePredicate: the sweep is derived from the
// record and the clock, and an ACTIVE record is never swept.
func TestPeerStoreTombstoneRetentionIsAPurePredicate(t *testing.T) {
	base := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	var now time.Time
	st, _ := psStore(t, func(o *PeerStoreOptions) {
		o.TombstoneRetention = time.Hour
		o.Now = func() time.Time { return now }
	})
	now = base
	if err := st.Apply(psCommitted(t, psRoute("bus-ps-live", 1, psURLGen1, base), 10)); err != nil {
		t.Fatalf("Apply(active): %v", err)
	}
	if err := st.Apply(psCommitted(t, PeerRecord{BusID: psRemoteBus, ConfigSeq: 5, State: PeerRecordRemoved, UpdatedAt: base}, 12)); err != nil {
		t.Fatalf("Apply(tombstone): %v", err)
	}
	if _, ok := st.Lookup(psRemoteBus); !ok {
		t.Fatalf("the tombstone was not retained at all")
	}
	now = base.Add(time.Hour) // exactly at the horizon: still retained
	if _, ok := st.Lookup(psRemoteBus); !ok {
		t.Errorf("the tombstone was swept AT the retention horizon; the predicate is strictly past it")
	}
	now = base.Add(time.Hour + time.Nanosecond)
	if _, ok := st.Lookup(psRemoteBus); ok {
		t.Errorf("the tombstone outlived its retention")
	}
	if _, ok := st.Lookup("bus-ps-live"); !ok {
		t.Errorf("an ACTIVE record was swept; a configuration stays until an operator withdraws it")
	}
}

// ---------------------------------------------------------------------------
// PINNED: the crash
// ---------------------------------------------------------------------------

const (
	// envPeerCrashPoint selects where the child kills itself. Unset means "not a
	// crash child", which is what makes TestPeerStoreCrashChild a no-op skip in
	// an ordinary package run.
	envPeerCrashPoint = "RELAY_PEERSTORE_CRASH_POINT"
	// envPeerCrashDir is the data directory the child writes into: a t.TempDir()
	// belonging to the parent, so no run shares a data directory with another
	// and the tracked data/ dir is never touched.
	envPeerCrashDir = "RELAY_PEERSTORE_CRASH_DIR"

	// peerCrashRotatePostCommit: the child's PIN ROTATION is committed and
	// fsynced, and the process dies before PutTrust can fold it into memory,
	// before PutTrust returns, and before any operator is told it worked.
	peerCrashRotatePostCommit = "rotate-post-commit-pre-ack"
)

// TestPeerStoreCrashChild is the child half. It does NOTHING in a normal run.
func TestPeerStoreCrashChild(t *testing.T) {
	point := os.Getenv(envPeerCrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envPeerCrashPoint + " is unset")
	}
	dir := os.Getenv(envPeerCrashDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is unset", envPeerCrashPoint, point, envPeerCrashDir)
	}
	if point != peerCrashRotatePostCommit {
		t.Fatalf("child: unknown crash point %q", point)
	}

	// NO deferred Close and NO t.Cleanup on the log: a Close that ran would be
	// exactly the graceful shutdown this test exists to rule out.
	lg, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("child: wal.Open(%s): %v", dir, err)
	}
	st, err := NewPeerStore(PeerStoreOptions{BusID: psLocalBus, Durable: &psKillAfterCommit{l: lg}})
	if err != nil {
		t.Fatalf("child: NewPeerStore: %v", err)
	}
	if _, err := wal.Replay(lg.Path(), st.Apply); err != nil {
		t.Fatalf("child: replaying %s: %v", lg.Path(), err)
	}
	pins := st.PinnedKeys(psOriginBus)
	if len(pins) != 1 || !bytes.Equal(pins[0], psKey(1)) {
		t.Fatalf("child: recovered pins %x, want exactly the parent's key; there is nothing to rotate", pins)
	}
	if _, ok := st.Lookup(psRemoteBus); !ok {
		t.Fatalf("child: the parent's route was not recovered")
	}

	got, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(2)}})
	t.Fatalf("child: PutTrust returned (%+v, %v) but the durable log kills this process the instant the commit is fsynced; the crash was never injected", got, err)
}

// psKillAfterCommit is the honest post-commit, pre-ack kill.
//
// It delegates to the REAL *wal.Log.Write — the whole prepare, commit and fsync
// cycle — and kills the process before returning, so PutTrust never folds the
// rotation into memory and never returns a record. The rotation is on stable
// storage and NOTHING in this process, and no operator, knows it.
type psKillAfterCommit struct{ l *wal.Log }

func (k *psKillAfterCommit) Write(e wal.Entry) (wal.Committed, error) {
	// Asserted HERE rather than in the parent because this is the only place the
	// entry the store built can be seen BEFORE it is written. If the store
	// handed the log anything other than the rotation, the parent's "the
	// rotation is durable" assertion would be examining bytes that never meant
	// what it thinks.
	if e.Kind != BusTrustRecordKind {
		return wal.Committed{}, fmt.Errorf("child: the peer store handed the durable log an entry of kind %q, want %q", e.Kind, BusTrustRecordKind)
	}
	rec, err := DecodeBusTrustRecord(e.Body)
	if err != nil {
		return wal.Committed{}, fmt.Errorf("child: the entry the peer store handed the durable log does not decode as a trust record: %v", err)
	}
	if len(rec.SigningKeys) != 1 || !bytes.Equal(rec.SigningKeys[0], psKey(2)) {
		return wal.Committed{}, fmt.Errorf("child: the entry pins %x; this crash point exists to prove the ROTATION is durable, so it must be the rotation that is written", rec.SigningKeys)
	}
	c, err := k.l.Write(e)
	if err != nil {
		return wal.Committed{}, err
	}
	psKillSelf()
	return c, nil
}

// psKillSelf kills this process with SIGKILL. SIGKILL cannot be caught, blocked
// or ignored, so nothing deferred, buffered or graceful runs afterwards — which
// is the entire evidentiary value of this test over a polite Close.
func psKillSelf() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	panic("peer store crash test: SIGKILL to self did not kill the process")
}

// runPeerCrashChild re-execs this test binary at the given crash point and
// asserts the child really DIED ON SIGKILL rather than failing its own
// assertions — without that check a broken child would silently turn the parent
// into a test of a directory nothing was ever written to.
func runPeerCrashChild(t *testing.T, point, dir string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestPeerStoreCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(), envPeerCrashPoint+"="+point, envPeerCrashDir+"="+dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash child %q did not finish in time: %v\n--- child output ---\n%s", point, ctx.Err(), out.String())
	}
	// A child that failed its OWN assertions also exits non-zero, so "err != nil"
	// is not the assertion. The wait status is.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child %q: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s", point, err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child %q: wait status is %T, want syscall.WaitStatus", point, ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child %q exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s", point, ws.ExitStatus(), out.String())
	}
}

// psReplayCommitted reads the committed history straight off the log, read-only,
// without opening a writer on it. It is how the parent learns what the dying
// child actually got onto stable storage.
func psReplayCommitted(t *testing.T, dir string) []wal.Committed {
	t.Helper()
	var got []wal.Committed
	if _, err := wal.Replay(filepath.Join(dir, wal.WALFileName), func(c wal.Committed) error {
		got = append(got, c)
		return nil
	}); err != nil {
		t.Fatalf("replaying the log in %s: %v", dir, err)
	}
	return got
}

// TestPeerStoreSurvivesReplay is RELAY-10's acceptance evidence, and it is a
// REAL kill -9 rather than a graceful restart.
//
// "The code looks right" is not evidence for a durability claim, and neither is
// a polite Close: a graceful shutdown lets every deferred Close, buffer flush
// and runtime finaliser run, so it cannot tell a durable write from a lucky one.
// A SIGKILL is the only thing that proves none of them was load-bearing — the
// parent opens the exact bytes the dying process had put on stable storage, with
// nobody having tidied the tail on the way out.
//
// The crash point is the one that matters: AFTER a PIN ROTATION's commit fsync
// and BEFORE anything acknowledges it. PutTrust never returns, the operator sees
// a dead process rather than a result, and their natural next move is to run the
// command again. What must hold on the other side:
//
//	(a) the rotation record is on stable storage;
//	(b) a FRESH store rebuilt from that log serves the ROTATED pin, and still
//	    serves the route written before the crash;
//	(c) the pre-crash trust record, replayed again as a DUPLICATE — which
//	    recovery is allowed to produce when it rewrites a log around damage —
//	    does NOT put the pinned key back. That is the trust-anchor downgrade the
//	    monotonic sequence exists to refuse, tested with the real bytes off the
//	    crashed log rather than a synthetic fixture.
func TestPeerStoreSurvivesReplay(t *testing.T) {
	dir := t.TempDir()

	// --- Phase 1, in THIS process: a route and a pin ------------------------
	st, lg := psOpenStore(t, dir, nil, nil)
	route, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log before handing the directory to the child: %v", err)
	}

	// --- Phase 2: the child rotates the pin and is SIGKILLed at the commit ---
	runPeerCrashChild(t, peerCrashRotatePostCommit, dir)

	// --- (a) THE ROTATION IS ON STABLE STORAGE ------------------------------
	//
	// Without this the rest could pass just as happily against a directory where
	// the rotation never happened, and would prove nothing.
	committed := psReplayCommitted(t, dir)
	if len(committed) != 3 {
		t.Fatalf("the crashed log holds %d committed entries, want exactly 3 (the parent's route, the parent's pin, the child's rotation): the child died before its rotation was durable, so there is no post-commit crash to recover from", len(committed))
	}
	if committed[2].Entry.Kind != BusTrustRecordKind {
		t.Fatalf("the last committed entry has kind %q, want %q", committed[2].Entry.Kind, BusTrustRecordKind)
	}
	rotated, err := DecodeBusTrustRecord(committed[2].Entry.Body)
	if err != nil {
		t.Fatalf("the last committed entry does not decode as a trust record: %v", err)
	}
	if len(rotated.SigningKeys) != 1 || !bytes.Equal(rotated.SigningKeys[0], psKey(2)) {
		t.Fatalf("the rotation record pins %x, want the child's new key", rotated.SigningKeys)
	}
	preCrash, err := DecodeBusTrustRecord(committed[1].Entry.Body)
	if err != nil {
		t.Fatalf("the pre-crash trust entry does not decode: %v", err)
	}
	if rotated.ConfigSeq <= preCrash.ConfigSeq {
		t.Fatalf("the rotation was written at config_seq %d, at or below the pre-crash %d", rotated.ConfigSeq, preCrash.ConfigSeq)
	}

	// --- (b) RECOVERY: a FRESH store, rebuilt only from the crashed log -----
	st2, lg2 := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg2.Close() }()

	if got := lg2.Recovered().Applied; got != 3 {
		t.Fatalf("recovery applied %d committed entries, want 3", got)
	}
	recovered, ok := st2.LookupTrust(psOriginBus)
	if !ok {
		t.Fatalf("after recovering from a SIGKILL the pinned bus is not in the table at all; a process killed with -9 flushes nothing, so this state has to come off the durable log")
	}
	psAssertTrust(t, "the pins recovered from the crashed log", recovered, rotated)
	recoveredRoute, ok := st2.Lookup(psRemoteBus)
	if !ok {
		t.Fatalf("the route written before the crash was not recovered")
	}
	psAssertRoute(t, "the route recovered from the crashed log", recoveredRoute, route)

	// --- (c) THE DUPLICATED PRE-CRASH RECORD DOES NOT DOWNGRADE THE ANCHOR --
	//
	// Recovery is allowed to rewrite a log around damage, so "each record is
	// handed to Apply exactly once, in the order it was written" is not an
	// assumption this store may make. These are the REAL committed bytes of the
	// pre-crash pin, replayed a second time behind the rotation.
	if err := st2.Apply(committed[1]); err != nil {
		t.Fatalf("re-applying the pre-crash record returned %v; Apply must never return an error", err)
	}
	after := st2.PinnedKeys(psOriginBus)
	if len(after) != 1 || !bytes.Equal(after[0], psKey(2)) {
		t.Fatalf("after the pre-crash record was replayed a SECOND time the pins are %x, want the ROTATED key: a duplicated older record downgraded a trust anchor, and nothing downstream could tell that from a legitimate rotation", after)
	}
}
