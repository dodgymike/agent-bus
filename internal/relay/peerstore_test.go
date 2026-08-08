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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
//
// IT DELIBERATELY HAS NO Dir, and that is a coverage decision rather than an
// omission. RELAY-34's withdrawal floor is a STRONGER defence than the in-log
// ones that came before it (the tombstone, and busTable.sweptMax), so a fixture
// that had a Dir would let the floor answer first and SHADOW them — every test
// here written to prove sweptMax refuses a resurrected record would still pass
// while no longer exercising sweptMax at all. That is silent coverage loss of a
// defence both review gates once found missing, so the two are kept separable:
// this fixture proves the in-log defences, and the tests that need the floor ask
// for a Dir explicitly (psOpenStore does, since it shares the log's directory).
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
	// Dir is the SAME directory the log lives in — that is the contract, and it
	// is what puts peer-withdrawal-floor next to bus.wal where an operator (and
	// the tail-truncation test) can find it.
	o := PeerStoreOptions{BusID: psLocalBus, Dir: dir, Durable: durable}
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

	// peerCrashRevokePostCommit: the child REVOKES the pinned bus signing key
	// and dies the instant the tombstone's commit is fsynced — before
	// RemoveTrust folds it in, before it returns, and before any operator is
	// told the revocation worked. RELAY-34's crash point.
	peerCrashRevokePostCommit = "revoke-post-commit-pre-ack"
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

	// NO deferred Close and NO t.Cleanup on the log: a Close that ran would be
	// exactly the graceful shutdown this test exists to rule out.
	lg, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("child: wal.Open(%s): %v", dir, err)
	}

	var durable PeerDurableLog
	switch point {
	case peerCrashRotatePostCommit:
		durable = &psKillAfterCommit{l: lg}
	case peerCrashRevokePostCommit:
		durable = &psKillAfterRevokeCommit{l: lg, dir: dir}
	default:
		t.Fatalf("child: unknown crash point %q", point)
	}

	st, err := NewPeerStore(PeerStoreOptions{BusID: psLocalBus, Dir: dir, Durable: durable})
	if err != nil {
		t.Fatalf("child: NewPeerStore: %v", err)
	}
	if _, err := wal.Replay(lg.Path(), st.Apply); err != nil {
		t.Fatalf("child: replaying %s: %v", lg.Path(), err)
	}
	pins := st.PinnedKeys(psOriginBus)
	if len(pins) != 1 || !bytes.Equal(pins[0], psKey(1)) {
		t.Fatalf("child: recovered pins %x, want exactly the parent's key; there is nothing to rotate or revoke", pins)
	}
	if _, ok := st.Lookup(psRemoteBus); !ok {
		t.Fatalf("child: the parent's route was not recovered")
	}

	if point == peerCrashRevokePostCommit {
		// The floor must NOT exist yet: it is written by RemoveTrust, and if it
		// were already here the wrapper's ordering assertion would prove nothing.
		if _, err := os.Stat(filepath.Join(dir, PeerWithdrawalFloorFileName)); !os.IsNotExist(err) {
			t.Fatalf("child: %s already exists before the revocation (stat err %v); nothing has withdrawn anything yet", PeerWithdrawalFloorFileName, err)
		}
		got, err := st.RemoveTrust(psOriginBus)
		t.Fatalf("child: RemoveTrust returned (%+v, %v) but the durable log kills this process the instant the commit is fsynced; the crash was never injected", got, err)
	}

	got, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(2)}})
	t.Fatalf("child: PutTrust returned (%+v, %v) but the durable log kills this process the instant the commit is fsynced; the crash was never injected", got, err)
}

// psKillAfterRevokeCommit is RELAY-34's post-commit, pre-ack kill — and it also
// PROVES THE WRITE-AHEAD ORDERING, which is the property the whole fix rests on.
//
// It runs BEFORE the real *wal.Log.Write, so the durable withdrawal floor must
// ALREADY be on disk when it is entered: PeerStore.write fsyncs the floor before
// it hands the tombstone to the log. Asserting that here rather than in the
// parent is what makes it an ordering proof — from the parent, after the crash,
// both files exist and their order is unobservable.
type psKillAfterRevokeCommit struct {
	l   *wal.Log
	dir string
}

func (k *psKillAfterRevokeCommit) Write(e wal.Entry) (wal.Committed, error) {
	if e.Kind != BusTrustRecordKind {
		return wal.Committed{}, fmt.Errorf("child: the peer store handed the durable log an entry of kind %q, want %q", e.Kind, BusTrustRecordKind)
	}
	rec, err := DecodeBusTrustRecord(e.Body)
	if err != nil {
		return wal.Committed{}, fmt.Errorf("child: the entry the peer store handed the durable log does not decode as a trust record: %v", err)
	}
	if rec.State != PeerRecordRemoved || len(rec.SigningKeys) != 0 {
		return wal.Committed{}, fmt.Errorf("child: the entry is %s with %d pinned keys; this crash point exists to prove a REVOCATION survives, so it must be the tombstone that is written", rec.State, len(rec.SigningKeys))
	}

	// THE ORDERING ASSERTION. The floor is written AHEAD of the log entry, so it
	// is on stable storage at this instant — before a single byte of the
	// tombstone has reached the log, and therefore before anything a later
	// discard could take away.
	floors, ferr := readPeerWithdrawalFloors(filepath.Join(k.dir, PeerWithdrawalFloorFileName))
	if ferr != nil {
		return wal.Committed{}, fmt.Errorf("child: reading the withdrawal floor before the log write: %v", ferr)
	}
	if got := floors[trustTableToken][psOriginBus]; got != rec.ConfigSeq {
		return wal.Committed{}, fmt.Errorf("child: the durable withdrawal floor for %s is %d before the log write, want the tombstone's config_seq %d; the floor is NOT being written ahead of the log entry, which is the entire mechanism", psOriginBus, got, rec.ConfigSeq)
	}

	c, err := k.l.Write(e)
	if err != nil {
		return wal.Committed{}, err
	}
	psKillSelf()
	return c, nil
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

// ---------------------------------------------------------------------------
// RELAY-34: revocation must survive a WAL discard
// ---------------------------------------------------------------------------

// psWALPath is the log inside a data directory.
func psWALPath(dir string) string { return filepath.Join(dir, wal.WALFileName) }

// psTruncateTail chops n bytes off the end of the log — the exact damage the
// security gate used to reproduce RELAY-34, and a shape invariant 6 REQUIRES
// recovery to survive by discarding and booting anyway.
func psTruncateTail(t *testing.T, dir string, n int64) {
	t.Helper()
	fi, err := os.Stat(psWALPath(dir))
	if err != nil {
		t.Fatalf("stat %s: %v", psWALPath(dir), err)
	}
	if fi.Size() <= n {
		t.Fatalf("the log is only %d bytes; there is nothing to truncate", fi.Size())
	}
	if err := os.Truncate(psWALPath(dir), fi.Size()-n); err != nil {
		t.Fatalf("truncating %d bytes off %s: %v", n, psWALPath(dir), err)
	}
}

// TestPeerStoreTrustSurvivesATornWALTail is RELAY-34's acceptance evidence, and
// it is a REGRESSION test: the exact eight-byte tail truncation below made a
// REVOKED pinned bus signing key come back before the durable withdrawal floor
// existed. Confirmed RED against f1a787c before the fix.
//
// # Why this shape, and why every part of it is load-bearing
//
// The defect needed two things that are individually ordinary and jointly fatal:
//
//   - a REAL kill -9 at the revocation's commit, so nothing graceful can have
//     tidied anything up and the floor's write-ahead ordering is observed by the
//     dying process rather than inferred afterwards (psKillAfterRevokeCommit
//     asserts the floor is already on disk BEFORE the log write it wraps);
//   - a DISCARD of that revocation, because invariant 6 forbids refusing to boot
//     over a damaged log — so the bus must come up having thrown the tombstone
//     away, and the revocation must survive that anyway.
//
// Realistic triggers for the second are bit-rot, a torn write and a VM or
// filesystem snapshot rolled back past the revocation. None is adversarial, and
// a single-operator deployment suffers a snapshot rollback exactly as much as a
// hostile one — so "our deployment is one person over SSH" is not a mitigation
// and must not be read as one.
//
// What it proves, in order:
//
//	(a) the revocation is on stable storage AND the floor was written AHEAD of
//	    it (asserted inside the dying child);
//	(b) after the 8-byte truncation the log NO LONGER CARRIES the revocation —
//	    without this the rest passes against an intact log and proves nothing;
//	(c) recovery still REACHES A RUNNING STORE (invariant 6): it does not refuse;
//	(d) the revoked key is STILL REVOKED, through every read path;
//	(e) the discard was LOGGED, not silent;
//	(f) the route written before the crash is untouched — the floor must not
//	    fail closed over configuration nobody withdrew.
func TestPeerStoreTrustSurvivesATornWALTail(t *testing.T) {
	dir := t.TempDir()

	// --- Phase 1: a route, and a pin the operator will later revoke ---------
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

	// --- Phase 2: the child REVOKES and is SIGKILLed at the commit ----------
	runPeerCrashChild(t, peerCrashRevokePostCommit, dir)

	// --- (a) THE REVOCATION IS DURABLE, AND SO IS ITS FLOOR -----------------
	committed := psReplayCommitted(t, dir)
	if len(committed) != 3 {
		t.Fatalf("the crashed log holds %d committed entries, want exactly 3 (route, pin, revocation): the child died before its revocation was durable, so there is no post-commit crash to recover from", len(committed))
	}
	revocation, err := DecodeBusTrustRecord(committed[2].Entry.Body)
	if err != nil {
		t.Fatalf("the last committed entry does not decode as a trust record: %v", err)
	}
	if revocation.State != PeerRecordRemoved {
		t.Fatalf("the last committed entry is %s, want a withdrawal tombstone", revocation.State)
	}
	floors, err := readPeerWithdrawalFloors(filepath.Join(dir, PeerWithdrawalFloorFileName))
	if err != nil {
		t.Fatalf("reading %s after the crash: %v", PeerWithdrawalFloorFileName, err)
	}
	if got := floors[trustTableToken][psOriginBus]; got != revocation.ConfigSeq {
		t.Fatalf("the durable withdrawal floor for %s is %d, want the revocation's config_seq %d; the crash left the floor and the log disagreeing", psOriginBus, got, revocation.ConfigSeq)
	}
	if got := len(floors[routeTableToken]); got != 0 {
		t.Fatalf("the route table has %d withdrawal floors, want 0; nothing withdrew a route", got)
	}

	// --- (b) DESTROY THE REVOCATION: eight bytes off the tail ---------------
	psTruncateTail(t, dir, 8)

	// --- (c) RECOVERY STILL REACHES A RUNNING STORE (invariant 6) ----------
	//
	// It does not refuse to boot, and it is wal.Open — not this test — that
	// repairs the tail, so what follows is measured against the bytes a real
	// recovery leaves behind.
	st2, lg2 := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg2.Close() }()
	if rec := lg2.Recovered(); len(rec.Dangling) == 0 && rec.DiscardCount == 0 {
		t.Fatalf("recovery reported neither a dangling prepare nor a discard (%+v); the truncation did not damage anything, so this test is no longer exercising the hole it exists for", rec)
	}

	// The revocation really is GONE from the log now. Without this the rest
	// would pass just as happily against an intact log and prove nothing.
	survivors := psReplayCommitted(t, dir)
	if len(survivors) != 2 {
		t.Fatalf("after recovery the log replays %d committed entries, want 2 (route, pin): the revocation was NOT discarded", len(survivors))
	}
	for _, c := range survivors {
		if c.Entry.Kind != BusTrustRecordKind {
			continue
		}
		rec, derr := DecodeBusTrustRecord(c.Entry.Body)
		if derr != nil {
			t.Fatalf("a surviving trust entry does not decode: %v", derr)
		}
		if rec.State == PeerRecordRemoved {
			t.Fatalf("the truncation did not remove the revocation from the log")
		}
	}

	sink := &psLogSink{}
	st3, err := NewPeerStore(PeerStoreOptions{BusID: psLocalBus, Dir: dir, Logger: logging.New(sink, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("NewPeerStore over the damaged directory: %v", err)
	}
	if _, err := wal.Replay(psWALPath(dir), st3.Apply); err != nil {
		t.Fatalf("replaying the damaged log: %v", err)
	}

	// --- (d) THE REVOKED KEY IS STILL REVOKED ------------------------------
	//
	// This is the assertion that was RED. Before the floor, the surviving
	// ACTIVE record was the only truth left and PinnedKeys returned the revoked
	// key, active, one key pinned.
	for _, tc := range []struct {
		name  string
		store *PeerStore
	}{
		{"a store recovered through wal.Open's applier", st2},
		{"a store recovered through wal.Replay", st3},
	} {
		if pins := tc.store.PinnedKeys(psOriginBus); len(pins) != 0 {
			t.Errorf("%s: PinnedKeys(%s) returned %x after a discarded revocation; A REVOKED PINNED BUS SIGNING KEY CAME BACK", tc.name, psOriginBus, pins)
		}
		if rec, ok := tc.store.LookupTrust(psOriginBus); ok {
			t.Errorf("%s: LookupTrust(%s) reported %+v; a bus whose pins were durably revoked must read as absent", tc.name, psOriginBus, rec)
		}
		for _, r := range tc.store.TrustedBuses() {
			if r.BusID == psOriginBus {
				t.Errorf("%s: TrustedBuses still lists %s", tc.name, psOriginBus)
			}
		}
	}

	// --- (e) THE DISCARD WAS NOT SILENT ------------------------------------
	if out := sink.String(); !strings.Contains(out, "NOT RESTORING a peer configuration") {
		t.Errorf("recovery did not log the superseded record; silent discard is the defect, not discard itself. Log was:\n%s", out)
	}

	// --- (f) NOTHING ELSE WAS FAILED CLOSED --------------------------------
	got, ok := st3.Lookup(psRemoteBus)
	if !ok {
		t.Fatalf("the route written before the crash was not recovered; the withdrawal floor must not fail closed over a configuration nobody withdrew")
	}
	psAssertRoute(t, "the route recovered from the damaged log", got, route)
}

// TestPeerStoreRePinsAfterADiscardedRevocation is the other half of RELAY-34's
// contract, and it is the half a floor gets wrong if it is bolted on carelessly:
// a revocation that STICKS must still be REVERSIBLE by the operator.
//
// Two traps are pinned here, both of which a naive floor walks straight into:
//
//   - Re-pinning the SAME key must not be swallowed as a no-op. PutTrust returns
//     early when the incoming pin set equals the current ACTIVE record — and
//     after a discarded revocation that record is still sitting in the table,
//     invisible but present. An operator would be told "nothing to do" and left
//     with an un-pinned bus.
//   - The new record must be minted ABOVE the floor. config_seq is otherwise
//     rebuilt only from the log, so a directory whose log lost the revocation
//     could mint below its own floor and refuse its own write for ever.
func TestPeerStoreRePinsAfterADiscardedRevocation(t *testing.T) {
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)
	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	if _, err := st.RemoveTrust(psOriginBus); err != nil {
		t.Fatalf("RemoveTrust: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	psTruncateTail(t, dir, 8)

	st2, lg2 := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg2.Close() }()

	// ANTI-VACUITY. Without these this test passed with the truncation removed
	// entirely, while its name, doc and failure strings all claimed to be testing
	// the state "after a discarded revocation" — the reviewer gate found it. The
	// discard has to be shown to have happened before anything is concluded from
	// it.
	if rec := lg2.Recovered(); len(rec.Dangling) == 0 && rec.DiscardCount == 0 {
		t.Fatalf("recovery reported neither a dangling prepare nor a discard (%+v); nothing was damaged, so this is not the state this test claims to exercise", rec)
	}
	for _, c := range psReplayCommitted(t, dir) {
		if c.Entry.Kind != BusTrustRecordKind {
			continue
		}
		r, derr := DecodeBusTrustRecord(c.Entry.Body)
		if derr != nil {
			t.Fatalf("decoding a surviving trust entry: %v", derr)
		}
		if r.State == PeerRecordRemoved {
			t.Fatalf("the revocation is still in the log; it was not discarded, so the floor is not what is keeping the key revoked below")
		}
	}

	if pins := st2.PinnedKeys(psOriginBus); len(pins) != 0 {
		t.Fatalf("the revoked key came back: %x", pins)
	}

	// The operator re-pins THE SAME KEY. This must write a new generation, not
	// return the invisible superseded one as a no-op.
	rec, err := st2.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}})
	if err != nil {
		t.Fatalf("re-pinning after a discarded revocation: %v", err)
	}
	if rec.State != PeerRecordActive {
		t.Fatalf("the re-pin produced a %s record", rec.State)
	}
	floors, err := readPeerWithdrawalFloors(filepath.Join(dir, PeerWithdrawalFloorFileName))
	if err != nil {
		t.Fatalf("reading the floor: %v", err)
	}
	if floor := floors[trustTableToken][psOriginBus]; rec.ConfigSeq <= floor {
		t.Fatalf("the re-pin was minted at config_seq %d, at or below the withdrawal floor %d; it would be refused by the floor it was written under, for ever", rec.ConfigSeq, floor)
	}
	pins := st2.PinnedKeys(psOriginBus)
	if len(pins) != 1 || !bytes.Equal(pins[0], psKey(1)) {
		t.Fatalf("after re-pinning, PinnedKeys = %x, want the operator's key back", pins)
	}

	// And it survives a restart, floor and all.
	if err := lg2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st3, lg3 := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg3.Close() }()
	pins = st3.PinnedKeys(psOriginBus)
	if len(pins) != 1 || !bytes.Equal(pins[0], psKey(1)) {
		t.Fatalf("after a restart the re-pinned key is %x, want it still pinned; the floor is refusing a generation written ABOVE it", pins)
	}
}

// TestPeerStoreRefusesAWithdrawalItCannotFloor pins the fail-CLOSED choice: a
// store that cannot write the durable floor REFUSES to withdraw, rather than
// recording a revocation only in the log and telling the operator it worked.
//
// This is also the hand-off contract for whoever wires a PeerStore up — most
// immediately the offline `agent-bus peer` subcommand, which is where an
// operator's revocation is actually typed. Forget PeerStoreOptions.Dir and the
// revocation path fails loudly at the first attempt instead of silently
// producing a revocation a torn tail can undo.
func TestPeerStoreRefusesAWithdrawalItCannotFloor(t *testing.T) {
	dir := t.TempDir()
	d := &psLateLog{}
	sink := &psLogSink{}
	st, err := NewPeerStore(PeerStoreOptions{BusID: psLocalBus, Durable: d, Logger: logging.New(sink, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("NewPeerStore: %v", err)
	}
	lg, err := wal.Open(wal.LogOptions{Dir: dir, Applier: st})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = lg.Close() }()
	d.l = lg

	// A store that can write but cannot floor says so at CONSTRUCTION, once,
	// rather than only at the moment a revocation is attempted.
	if out := sink.String(); !strings.Contains(out, "WITHDRAWALS WILL BE REFUSED") {
		t.Errorf("a durable store with no data directory did not warn at construction. Log was:\n%s", out)
	}

	if _, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1}); err != nil {
		t.Fatalf("Put on a store with no data directory: %v; only WITHDRAWALS are refused", err)
	}
	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("PutTrust on a store with no data directory: %v; only WITHDRAWALS are refused", err)
	}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Remove", func() error { _, err := st.Remove(psRemoteBus); return err }},
		{"RemoveTrust", func() error { _, err := st.RemoveTrust(psOriginBus); return err }},
	} {
		err := tc.call()
		if !errors.Is(err, ErrPeerNoWithdrawalFloor) {
			t.Errorf("%s returned %v, want ErrPeerNoWithdrawalFloor", tc.name, err)
		}
	}

	// AND NOTHING WAS WRITTEN. A refusal that had already appended a tombstone
	// would be the worst of both: the operator is told it failed and the log
	// says it happened.
	for _, c := range psReplayCommitted(t, dir) {
		var state PeerRecordState
		switch c.Entry.Kind {
		case PeerRecordKind:
			rec, derr := DecodePeerRecord(c.Entry.Body)
			if derr != nil {
				t.Fatalf("decoding a committed route: %v", derr)
			}
			state = rec.State
		case BusTrustRecordKind:
			rec, derr := DecodeBusTrustRecord(c.Entry.Body)
			if derr != nil {
				t.Fatalf("decoding a committed trust record: %v", derr)
			}
			state = rec.State
		default:
			continue
		}
		if state == PeerRecordRemoved {
			t.Errorf("a withdrawal that was REFUSED still reached the durable log")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, PeerWithdrawalFloorFileName)); !os.IsNotExist(err) {
		t.Errorf("a store with no data directory wrote %s anyway (stat err %v)", PeerWithdrawalFloorFileName, err)
	}
}

// TestPeerWithdrawalFloorFileIsStrictlyVerified pins the file format itself. It
// is the trust anchor's trust anchor: a floor read out of bytes that are not the
// bytes that were written could FORGET a revocation, which is the one thing the
// file exists to make impossible. So every failure is FATAL and the file is
// never regenerated.
func TestPeerWithdrawalFloorFileIsStrictlyVerified(t *testing.T) {
	good, err := encodePeerWithdrawalFloors([]peerWithdrawalEntry{
		{table: trustTableToken, busID: psOriginBus, seq: 7},
		{table: routeTableToken, busID: psRemoteBus, seq: 3},
	})
	if err != nil {
		t.Fatalf("encodePeerWithdrawalFloors: %v", err)
	}
	// The canonical form is readable by eye and sorted, so the bytes are a
	// function of the withdrawal set alone.
	wantBody := "route " + psRemoteBus + " 3\ntrust " + psOriginBus + " 7\n"
	if !strings.HasSuffix(string(good), wantBody) {
		t.Errorf("the encoded body is not the canonical sorted form; got:\n%s", good)
	}
	if !strings.HasPrefix(string(good), peerWithdrawalFloorMagic+" v6 sha256=") {
		t.Errorf("the header is not %q v6 with a sha256 digest; got:\n%s", peerWithdrawalFloorMagic, good)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, PeerWithdrawalFloorFileName)
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	floors, err := readPeerWithdrawalFloors(path)
	if err != nil {
		t.Fatalf("readPeerWithdrawalFloors on a good file: %v", err)
	}
	if floors[trustTableToken][psOriginBus] != 7 || floors[routeTableToken][psRemoteBus] != 3 {
		t.Fatalf("round trip lost a floor: %+v", floors)
	}

	sum := func(body string) string {
		h := sha256.Sum256([]byte(body))
		return peerWithdrawalFloorMagic + " v6 sha256=" + hex.EncodeToString(h[:]) + "\n" + body
	}
	for _, tc := range []struct{ name, content string }{
		{"no header line", peerWithdrawalFloorMagic},
		{"a foreign magic", sum("trust " + psOriginBus + " 7\n")[len(peerWithdrawalFloorMagic):]},
		// A VALID digest over the body, so the version check is what refuses it.
		// This case used to carry "sha256=00", which the digest-LENGTH check
		// refused first — so deleting the version check entirely left the suite
		// green, while CONTRACTS-ONDISK.md cited this list as evidence it was
		// covered. Reviewer-gate finding.
		{"an unknown on-disk format version", strings.Replace(sum("trust "+psOriginBus+" 7\n"), " v6 ", " v99 ", 1)},
		{"a body that does not match the digest", peerWithdrawalFloorMagic + " v6 sha256=" + strings.Repeat("00", 32) + "\ntrust " + psOriginBus + " 7\n"},
		{"an unknown table token", sum("pins " + psOriginBus + " 7\n")},
		{"an unfolded bus id", sum("trust BUS-PS-ORIGIN 7\n")},
		{"an invalid bus id", sum("trust bus.with.dots 7\n")},
		{"a zero floor", sum("trust " + psOriginBus + " 0\n")},
		{"a leading zero", sum("trust " + psOriginBus + " 07\n")},
		{"a floor above the plausibility bound", sum("trust " + psOriginBus + " 9007199254740992\n")},
		{"a duplicated (table, bus)", sum("trust " + psOriginBus + " 7\ntrust " + psOriginBus + " 2\n")},
		{"a short line", sum("trust " + psOriginBus + "\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), PeerWithdrawalFloorFileName)
			if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			_, err := readPeerWithdrawalFloors(p)
			if !errors.Is(err, ErrPeerWithdrawalFloorCorrupt) {
				t.Fatalf("readPeerWithdrawalFloors returned %v, want ErrPeerWithdrawalFloorCorrupt", err)
			}
			// The remedy is part of the contract: without it this error is a
			// permanently unstartable bus, which invariant 6 forbids. Matched
			// case-insensitively so a reworded sentence cannot fail the test for
			// the wrong reason — what must hold is that the PATH and the ACTION
			// are both named, not the capitalisation.
			if !strings.Contains(strings.ToLower(err.Error()), "move "+strings.ToLower(p)+" aside") {
				t.Errorf("the fatal error does not name the one-step remedy: %v", err)
			}
		})
	}

	// A MISSING file is legal and means "nothing has ever been withdrawn". It is
	// NOT fatal, because there is no data directory in existence whose
	// withdrawals predate this file — nothing outside internal/relay had ever
	// constructed a PeerStore when it landed.
	missing, err := readPeerWithdrawalFloors(filepath.Join(t.TempDir(), PeerWithdrawalFloorFileName))
	if err != nil {
		t.Fatalf("a missing floor file is fatal: %v", err)
	}
	if len(missing[routeTableToken]) != 0 || len(missing[trustTableToken]) != 0 {
		t.Errorf("a missing floor file yielded floors: %+v", missing)
	}
}

// TestPeerStoreSeedsConfigSeqFromTheWithdrawalFloor pins the recovery direction
// the floor makes possible ON ITS OWN: a data directory whose LOG is gone but
// whose floor survived must not resume minting below its own floor.
//
// Without the seeding this is not a cosmetic gap. Every PutTrust for a
// previously-revoked bus would be minted at 1, refused by the floor, and retried
// at 1 again — a bus that can never be re-pinned, for ever, with an error
// message blaming a revocation the operator already knows about.
func TestPeerStoreSeedsConfigSeqFromTheWithdrawalFloor(t *testing.T) {
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)
	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	revocation, err := st.RemoveTrust(psOriginBus)
	if err != nil {
		t.Fatalf("RemoveTrust: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The whole log is lost — a quarantine, a restore from a backup that predates
	// it, an operator moving it aside. The floor file is all that is left.
	if err := os.Remove(psWALPath(dir)); err != nil {
		t.Fatalf("removing the log: %v", err)
	}

	st2, lg2 := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg2.Close() }()
	rec, err := st2.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(2)}})
	if err != nil {
		t.Fatalf("re-pinning on a directory whose log is gone: %v; the store is minting below its own withdrawal floor", err)
	}
	if rec.ConfigSeq <= revocation.ConfigSeq {
		t.Fatalf("the new record is at config_seq %d, at or below the surviving withdrawal floor %d", rec.ConfigSeq, revocation.ConfigSeq)
	}
	if pins := st2.PinnedKeys(psOriginBus); len(pins) != 1 || !bytes.Equal(pins[0], psKey(2)) {
		t.Fatalf("PinnedKeys = %x, want the newly pinned key", pins)
	}
}

// TestPeerStoreRepairsALostWithdrawalFloorFromTheLog is a REGRESSION test for
// the security gate's P1 on RELAY-34: the fix's own remedy silently re-opened
// the hole it closed.
//
// The state at issue is "tombstone in the log, no floor beside it", and it has
// three non-adversarial ways in — bit-rot on the floor file, an inconsistent
// snapshot or backup restore that brings bus.wal forward without it, and an
// operator following ErrPeerWithdrawalFloorCorrupt's own instruction to move it
// aside. In that state the pre-RELAY-34 behaviour was back: an 8-byte tail
// truncation resurrected the revoked key.
//
// Two arms, because the gate found two distinct failures:
//
//	(1) THE BUS REPAIRS ITSELF on the next start, from the withdrawal its log
//	    still holds — so the remedy works without the operator doing anything;
//	(2) RE-APPLYING the withdrawal by hand — the documented remedy — actually
//	    writes the floor, where it used to take a no-op branch, return success
//	    and write nothing.
func TestPeerStoreRepairsALostWithdrawalFloorFromTheLog(t *testing.T) {
	floorOf := func(t *testing.T, dir string) uint64 {
		t.Helper()
		floors, err := readPeerWithdrawalFloors(filepath.Join(dir, PeerWithdrawalFloorFileName))
		if err != nil {
			t.Fatalf("reading the floor: %v", err)
		}
		return floors[trustTableToken][psOriginBus]
	}

	// setup writes a pin and revokes it, then DESTROYS the floor file while
	// leaving the log — and its tombstone — intact.
	setup := func(t *testing.T) (string, uint64) {
		t.Helper()
		dir := t.TempDir()
		st, lg := psOpenStore(t, dir, nil, nil)
		if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
			t.Fatalf("PutTrust: %v", err)
		}
		rev, err := st.RemoveTrust(psOriginBus)
		if err != nil {
			t.Fatalf("RemoveTrust: %v", err)
		}
		if err := lg.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := floorOf(t, dir); got != rev.ConfigSeq {
			t.Fatalf("the floor is %d before it is destroyed, want %d", got, rev.ConfigSeq)
		}
		if err := os.Remove(filepath.Join(dir, PeerWithdrawalFloorFileName)); err != nil {
			t.Fatalf("removing the floor file: %v", err)
		}
		return dir, rev.ConfigSeq
	}

	t.Run("recovery rebuilds it from the log", func(t *testing.T) {
		dir, want := setup(t)

		sink := &psLogSink{}
		st, lg := psOpenStore(t, dir, func(o *PeerStoreOptions) {
			o.Logger = logging.New(sink, logging.LevelDebug)
		}, nil)
		if err := lg.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := floorOf(t, dir); got != want {
			t.Fatalf("after a restart the withdrawal floor is %d, want it REBUILT to %d from the tombstone the log still holds", got, want)
		}
		if pins := st.PinnedKeys(psOriginBus); len(pins) != 0 {
			t.Fatalf("PinnedKeys = %x after recovery, want none", pins)
		}
		if out := sink.String(); !strings.Contains(out, "REPAIRED the durable withdrawal floor") {
			t.Errorf("the repair was not logged; a floor rebuilt in silence is a floor nobody knows was ever lost. Log was:\n%s", out)
		}

		// AND THE REBUILT FLOOR HOLDS AGAINST THE ORIGINAL ATTACK. This is the
		// assertion that was RED: without the repair the 8-byte truncation
		// brought the revoked key straight back.
		psTruncateTail(t, dir, 8)
		st2, lg2 := psOpenStore(t, dir, nil, nil)
		defer func() { _ = lg2.Close() }()
		if pins := st2.PinnedKeys(psOriginBus); len(pins) != 0 {
			t.Fatalf("after the floor was lost and rebuilt, an 8-byte tail truncation returned the REVOKED key %x", pins)
		}
	})

	t.Run("re-applying the withdrawal by hand re-asserts the floor", func(t *testing.T) {
		dir, want := setup(t)

		// A store that has NOT replayed the log, so the automatic repair cannot
		// have run: this arm isolates the operator's manual remedy.
		d := &psLateLog{}
		st, err := NewPeerStore(PeerStoreOptions{BusID: psLocalBus, Dir: dir, Durable: d})
		if err != nil {
			t.Fatalf("NewPeerStore: %v", err)
		}
		lg, err := wal.Open(wal.LogOptions{Dir: dir})
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		defer func() { _ = lg.Close() }()
		d.l = lg
		if _, err := wal.Replay(lg.Path(), func(c wal.Committed) error {
			// Deliberately folded WITHOUT Apply's reconciliation, so the floor is
			// still missing when RemoveTrust is called below.
			s := st
			s.mu.Lock()
			defer s.mu.Unlock()
			switch c.Entry.Kind {
			case BusTrustRecordKind:
				rec, derr := DecodeBusTrustRecord(c.Entry.Body)
				if derr != nil {
					return nil
				}
				_ = s.applyLocked(s.trust, rec, "test")
			}
			return nil
		}); err != nil {
			t.Fatalf("replaying: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, PeerWithdrawalFloorFileName)); !os.IsNotExist(err) {
			t.Fatalf("the floor exists before the manual remedy (stat err %v); this arm proves nothing", err)
		}

		// THE DOCUMENTED REMEDY. It used to return success and write nothing.
		if _, err := st.RemoveTrust(psOriginBus); err != nil {
			t.Fatalf("re-applying the withdrawal by hand: %v", err)
		}
		if got := floorOf(t, dir); got != want {
			t.Fatalf("re-applying the withdrawal left the floor at %d, want %d; the remedy reported success and wrote nothing", got, want)
		}
	})
}

// TestPeerWithdrawalFloorRefusesAnImplausibleSequence is a REGRESSION test for
// the security gate's P2 on RELAY-34.
//
// NewPeerStore seeds config_seq from the floors, so a single planted entry near
// maxConfigSeq seeded the counter to its ceiling: the bus started perfectly
// healthy, read fine, and then failed EVERY Put, PutTrust, Remove and
// RemoveTrust — for every bus, in BOTH tables, across every restart — with
// "configuration sequence exhausted", naming a ceiling nobody had reached.
// Total loss of function with the diagnosis pointing somewhere else entirely.
func TestPeerWithdrawalFloorRefusesAnImplausibleSequence(t *testing.T) {
	plant := func(t *testing.T, seq uint64) string {
		t.Helper()
		dir := t.TempDir()
		body := "trust " + psOriginBus + " " + strconv.FormatUint(seq, 10) + "\n"
		h := sha256.Sum256([]byte(body))
		data := peerWithdrawalFloorMagic + " v6 sha256=" + hex.EncodeToString(h[:]) + "\n" + body
		if err := os.WriteFile(filepath.Join(dir, PeerWithdrawalFloorFileName), []byte(data), 0o600); err != nil {
			t.Fatalf("planting: %v", err)
		}
		return dir
	}

	for _, seq := range []uint64{maxConfigSeq, maxConfigSeq - 1, maxPlausiblePeerWithdrawalSeq} {
		dir := plant(t, seq)
		_, err := NewPeerStore(PeerStoreOptions{BusID: psLocalBus, Dir: dir})
		if !errors.Is(err, ErrPeerWithdrawalFloorCorrupt) {
			t.Errorf("a planted floor of %d was ACCEPTED (err %v); it would seed config_seq near its ceiling and permanently refuse every write in both tables", seq, err)
		}
	}

	// Just below the bound is still adopted: the bound separates "tampered" from
	// "high", and must not refuse a value a bus could genuinely reach.
	dir := plant(t, maxPlausiblePeerWithdrawalSeq-1)
	st, err := NewPeerStore(PeerStoreOptions{BusID: psLocalBus, Dir: dir})
	if err != nil {
		t.Fatalf("a floor just below the plausibility bound was refused: %v", err)
	}
	if pins := st.PinnedKeys(psOriginBus); len(pins) != 0 {
		t.Errorf("PinnedKeys = %x, want none", pins)
	}
}

// ---------------------------------------------------------------------------
// RELAY-34: the mutants the reviewer gate found alive
// ---------------------------------------------------------------------------
//
// Every test below was written because a MUTATION of the shipped mechanism left
// the whole suite green. A defence nothing can detect the absence of is a
// defence that will be deleted by a future edit and nobody will know, which for
// this mechanism means a revoked pinned bus signing key silently comes back. Each
// names the mutation it kills.

// psFloors reads the floor file, requiring it to exist and verify.
func psFloors(t *testing.T, dir string) map[string]map[string]uint64 {
	t.Helper()
	floors, err := readPeerWithdrawalFloors(filepath.Join(dir, PeerWithdrawalFloorFileName))
	if err != nil {
		t.Fatalf("reading %s: %v", PeerWithdrawalFloorFileName, err)
	}
	return floors
}

// TestPeerWithdrawalFloorAccumulatesEveryWithdrawal kills the mutant that
// deletes `t.withdrawnAt[folded] = seq` in recordWithdrawal.
//
// The blind spot it closes: NO other test performs more than ONE withdrawal
// against a single store. recordWithdrawal rebuilds the WHOLE file from the
// in-memory mirror on every call, so if the mirror is not updated, withdrawal N
// ERASES withdrawals 1..N-1 from disk — RELAY-34's own fail-open, reintroduced,
// with a green suite. The reviewer gate demonstrated exactly that.
func TestPeerWithdrawalFloorAccumulatesEveryWithdrawal(t *testing.T) {
	const busA, busB = "bus-ps-alpha", "bus-ps-beta"
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)

	if _, err := st.PutTrust(BusTrust{BusID: busA, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("PutTrust(A): %v", err)
	}
	if _, err := st.PutTrust(BusTrust{BusID: busB, SigningKeys: []ed25519.PublicKey{psKey(2)}}); err != nil {
		t.Fatalf("PutTrust(B): %v", err)
	}
	if _, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	revA, err := st.RemoveTrust(busA)
	if err != nil {
		t.Fatalf("RemoveTrust(A): %v", err)
	}
	revB, err := st.RemoveTrust(busB)
	if err != nil {
		t.Fatalf("RemoveTrust(B): %v", err)
	}
	revRoute, err := st.Remove(psRemoteBus)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// ALL THREE must be on disk simultaneously, at their exact sequences. The
	// mutant leaves only the last.
	floors := psFloors(t, dir)
	for _, want := range []struct {
		table, bus string
		seq        uint64
	}{
		{trustTableToken, busA, revA.ConfigSeq},
		{trustTableToken, busB, revB.ConfigSeq},
		{routeTableToken, psRemoteBus, revRoute.ConfigSeq},
	} {
		if got := floors[want.table][want.bus]; got != want.seq {
			t.Errorf("%s floor for %s is %d, want %d; an earlier withdrawal was ERASED by a later one, which is RELAY-34's fail-open reintroduced (floors: %+v)", want.table, want.bus, got, want.seq, floors)
		}
	}

	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// And the consequence, end to end: after a discard BOTH revocations hold.
	psTruncateTail(t, dir, 8)
	st2, lg2 := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg2.Close() }()
	for _, bus := range []string{busA, busB} {
		if pins := st2.PinnedKeys(bus); len(pins) != 0 {
			t.Errorf("after a tail truncation, PinnedKeys(%s) = %x; a REVOKED key came back", bus, pins)
		}
	}
	for _, r := range st2.ActivePeers() {
		if r.BusID == psRemoteBus {
			t.Errorf("ActivePeers still lists the withdrawn route %s", psRemoteBus)
		}
	}
}

// psFailWriteOfKind wraps a durable log and FAILS the write for one entry kind,
// after the store has already fsynced the withdrawal floor.
//
// psOpenStore has always taken a wrap func and NO test used it to fail a write
// (the reviewer gate's finding). This is the window the read-side and
// write-side floor checks exist for and which nothing exercised.
type psFailWriteOfKind struct {
	inner PeerDurableLog
	kind  string
	state PeerRecordState
	fired int32
}

func (w *psFailWriteOfKind) Write(e wal.Entry) (wal.Committed, error) {
	if e.Kind == w.kind {
		var state PeerRecordState
		switch e.Kind {
		case PeerRecordKind:
			rec, err := DecodePeerRecord(e.Body)
			if err != nil {
				return wal.Committed{}, err
			}
			state = rec.State
		case BusTrustRecordKind:
			rec, err := DecodeBusTrustRecord(e.Body)
			if err != nil {
				return wal.Committed{}, err
			}
			state = rec.State
		}
		if state == w.state {
			atomic.AddInt32(&w.fired, 1)
			return wal.Committed{}, errors.New("psFailWriteOfKind: injected durable-log failure")
		}
	}
	return w.inner.Write(e)
}

// TestPeerStoreHidesAPinWhoseFloorLandedButWhoseTombstoneDidNot kills THREE
// mutants at once, all of which the reviewer gate found alive:
//
//   - removing the floor check from busTable.lookup;
//   - removing it from ActivePeers and TrustedBuses;
//   - removing the `if known && t.withdrawn(existing) { known = false }` reset in
//     PeerStore.write.
//
// They were dead code as far as the suite was concerned because upsert refuses
// the superseded record at ADMISSION, so it never reached the table in any
// existing test. The window where they are the ONLY defence is precise: the
// withdrawal floor is fsynced and THEN the durable log write fails. The table
// still holds the live generation, nothing was refused at admission, and only
// the read-side and write-side checks stop a revoked key being served.
//
// The file documented that window in prose and nothing exercised it.
func TestPeerStoreHidesAPinWhoseFloorLandedButWhoseTombstoneDidNot(t *testing.T) {
	dir := t.TempDir()
	fail := &psFailWriteOfKind{kind: BusTrustRecordKind, state: PeerRecordRemoved}
	st, lg := psOpenStore(t, dir, nil, func(inner PeerDurableLog) PeerDurableLog {
		fail.inner = inner
		return fail
	})
	defer func() { _ = lg.Close() }()

	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	if _, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// The revocation's FLOOR lands; its LOG ENTRY does not.
	if _, err := st.RemoveTrust(psOriginBus); err == nil {
		t.Fatalf("RemoveTrust succeeded; the injected failure did not fire")
	}
	if atomic.LoadInt32(&fail.fired) != 1 {
		t.Fatalf("the injected failure fired %d times, want 1", fail.fired)
	}
	rev := psFloors(t, dir)[trustTableToken][psOriginBus]
	if rev == 0 {
		t.Fatalf("the withdrawal floor was not written before the log write; the whole write-ahead ordering is inverted")
	}
	// The tombstone really is NOT in the log — otherwise this proves nothing.
	for _, c := range psReplayCommitted(t, dir) {
		if c.Entry.Kind != BusTrustRecordKind {
			continue
		}
		r, err := DecodeBusTrustRecord(c.Entry.Body)
		if err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if r.State == PeerRecordRemoved {
			t.Fatalf("the tombstone reached the log; the failure was not injected where this test needs it")
		}
	}

	// EVERY read path must report the bus as un-pinned, from the LIVE store —
	// with the superseded ACTIVE record still sitting in the table.
	if pins := st.PinnedKeys(psOriginBus); len(pins) != 0 {
		t.Errorf("PinnedKeys = %x; the floor landed, so the key is REVOKED even though its tombstone never reached the log", pins)
	}
	if rec, ok := st.LookupTrust(psOriginBus); ok {
		t.Errorf("LookupTrust reported %+v, want absent", rec)
	}
	for _, r := range st.TrustedBuses() {
		if r.BusID == psOriginBus {
			t.Errorf("TrustedBuses still lists %s", psOriginBus)
		}
	}

	// And the WRITE path must treat it as absent too: re-pinning the SAME key
	// must write a new generation above the floor rather than being swallowed as
	// a no-op against the hidden record.
	rec, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}})
	if err != nil {
		t.Fatalf("re-pinning: %v", err)
	}
	if rec.ConfigSeq <= rev {
		t.Fatalf("the re-pin was minted at config_seq %d, at or below the floor %d", rec.ConfigSeq, rev)
	}
	if pins := st.PinnedKeys(psOriginBus); len(pins) != 1 || !bytes.Equal(pins[0], psKey(1)) {
		t.Fatalf("after re-pinning, PinnedKeys = %x, want the key back", pins)
	}

	// The ROUTE table's list path, same window, so ActivePeers is covered too.
	routeFail := &psFailWriteOfKind{kind: PeerRecordKind, state: PeerRecordRemoved, inner: fail.inner}
	st.durable = routeFail
	if _, err := st.Remove(psRemoteBus); err == nil {
		t.Fatalf("Remove succeeded; the injected route failure did not fire")
	}
	if _, ok := st.Lookup(psRemoteBus); ok {
		t.Errorf("Lookup still reports the withdrawn route %s", psRemoteBus)
	}
	for _, r := range st.ActivePeers() {
		if r.BusID == psRemoteBus {
			t.Errorf("ActivePeers still lists the withdrawn route %s", psRemoteBus)
		}
	}
}

// TestPeerWithdrawalFloorIsWrittenAtomically kills the mutant that replaces
// atomicReplacePeerWithdrawalFloor with a bare os.WriteFile.
//
// The discriminator is deliberate rather than incidental. A bare os.WriteFile
// opens the EXISTING file O_TRUNC, which needs write permission on the FILE
// (0600, ours) and not on the directory — so on a READ-ONLY directory it
// truncates the floor to nothing and only then fails, DESTROYING every
// revocation already recorded. The atomic writer creates its temp file in the
// directory first, so it fails before touching anything.
//
// It also asserts the mode the writer produced (a dropped tmp.Chmod is
// otherwise invisible — every other test writes its own 0600 fixtures) and that
// no temp file is left behind.
func TestPeerWithdrawalFloorIsWrittenAtomically(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not restrain root, so the discriminator does not hold")
	}
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)
	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	if _, err := st.PutTrust(BusTrust{BusID: psRemoteBus, SigningKeys: []ed25519.PublicKey{psKey(2)}}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	first, err := st.RemoveTrust(psOriginBus)
	if err != nil {
		t.Fatalf("RemoveTrust: %v", err)
	}

	path := filepath.Join(dir, PeerWithdrawalFloorFileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("the floor file the writer produced is mode %o, want 0600; it holds no secret, but a writer that does not chmod before writing is a writer whose bytes were briefly world-readable", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".peer-withdrawal-floor-") {
			t.Errorf("a temp file %q was left behind by a SUCCESSFUL write", e.Name())
		}
	}

	// Now make the DIRECTORY unwritable and withdraw again.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()

	if _, err := st.RemoveTrust(psRemoteBus); err == nil {
		t.Fatalf("RemoveTrust succeeded on a read-only data directory; the floor cannot have been written, so the revocation must be REFUSED rather than acknowledged")
	}

	// THE ALREADY-RECORDED REVOCATION MUST BE UNHARMED. A non-atomic writer
	// truncates it here.
	floors, err := readPeerWithdrawalFloors(path)
	if err != nil {
		t.Fatalf("the existing floor file no longer verifies after a FAILED write: %v; the writer is not atomic, and a failed write has destroyed revocations it never touched", err)
	}
	if got := floors[trustTableToken][psOriginBus]; got != first.ConfigSeq {
		t.Fatalf("after a failed write the recorded floor for %s is %d, want it untouched at %d", psOriginBus, got, first.ConfigSeq)
	}
	_ = lg.Close()
}

// TestPeerWithdrawalFloorBoundsAreEnforced kills the mutants that remove the
// read-side size and entry-count bounds and the write-side withdrawal cap.
// The reviewer gate found ZERO test references to any of the three identifiers.
func TestPeerWithdrawalFloorBoundsAreEnforced(t *testing.T) {
	t.Run("an oversized file is refused without being read into memory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, PeerWithdrawalFloorFileName)
		big := make([]byte, maxPeerWithdrawalFloorFileSize+1)
		for i := range big {
			big[i] = 'a'
		}
		if err := os.WriteFile(path, big, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		_, err := readPeerWithdrawalFloors(path)
		if !errors.Is(err, ErrPeerWithdrawalFloorCorrupt) {
			t.Fatalf("a %d-byte floor file returned %v, want ErrPeerWithdrawalFloorCorrupt", len(big), err)
		}
		if !strings.Contains(err.Error(), "larger than") {
			t.Errorf("the refusal does not name the size bound: %v", err)
		}
	})

	t.Run("too many entries is refused", func(t *testing.T) {
		var body strings.Builder
		for i := 0; i <= maxPeerWithdrawalFloorEntries; i++ {
			fmt.Fprintf(&body, "trust bus-ps-%d %d\n", i, i+1)
		}
		h := sha256.Sum256([]byte(body.String()))
		data := peerWithdrawalFloorMagic + " v6 sha256=" + hex.EncodeToString(h[:]) + "\n" + body.String()
		path := filepath.Join(t.TempDir(), PeerWithdrawalFloorFileName)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		_, err := readPeerWithdrawalFloors(path)
		if !errors.Is(err, ErrPeerWithdrawalFloorCorrupt) {
			t.Fatalf("a floor file with %d entries returned %v, want ErrPeerWithdrawalFloorCorrupt", maxPeerWithdrawalFloorEntries+1, err)
		}
	})

	t.Run("the withdrawal cap refuses rather than growing the file without bound", func(t *testing.T) {
		dir := t.TempDir()
		st, lg := psOpenStore(t, dir, nil, nil)
		defer func() { _ = lg.Close() }()
		if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
			t.Fatalf("PutTrust: %v", err)
		}
		// The cap is 4096 withdrawals; reaching it through the API would be 4096
		// durable writes. The mirror is filled directly instead — this is an
		// in-package test and the cap is a property of the mirror's size, not of
		// how it got that big.
		st.mu.Lock()
		for i := 0; i < maxPeerWithdrawalFloorEntries; i++ {
			st.routes.withdrawnAt[fmt.Sprintf("bus-ps-filler-%d", i)] = uint64(i + 1)
		}
		st.mu.Unlock()

		_, err := st.RemoveTrust(psOriginBus)
		if !errors.Is(err, ErrTooManyPeerWithdrawals) {
			t.Fatalf("RemoveTrust at the withdrawal cap returned %v, want ErrTooManyPeerWithdrawals", err)
		}
		// AND THE REVOCATION MUST NOT HAVE HAPPENED: a refusal that had already
		// written the tombstone would be the worst of both.
		if pins := st.PinnedKeys(psOriginBus); len(pins) != 1 {
			t.Errorf("the refused revocation still un-pinned the bus (pins %x); it must be refused, not half-applied", pins)
		}
	})
}

// TestPeerStoreUpsertRefusesAWithdrawnRecordWithErrPeerWithdrawn asserts the
// PRIMARY recovery-time defence by its SENTINEL rather than by a log substring.
//
// The reviewer gate found that removing the floor check from busTable.upsert
// failed exactly one assertion — a strings.Contains on a log message — so a
// reword of that message would have turned the mechanism's only direct test into
// a false pass. errors.Is cannot be defeated by prose.
func TestPeerStoreUpsertRefusesAWithdrawnRecordWithErrPeerWithdrawn(t *testing.T) {
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	st, _ := psStore(t, nil)
	st.trust.withdrawnAt[psOriginBus] = 5
	st.routes.withdrawnAt[psRemoteBus] = 6

	for _, tc := range []struct {
		name    string
		table   *busTable
		rec     busScopedRecord
		refused bool
	}{
		{"an active trust record below the floor", st.trust, psTrust(psOriginBus, 4, at, psKey(1)), true},
		{"an active trust record AT the floor", st.trust, psTrust(psOriginBus, 5, at, psKey(1)), true},
		{"an active trust record above the floor", st.trust, psTrust(psOriginBus, 6, at, psKey(1)), false},
		{"an active route below the floor", st.routes, psRoute(psRemoteBus, 3, psURLGen1, at), true},
		{
			// A TOMBSTONE is never refused by the floor: the record that SET the
			// floor is itself a tombstone at exactly that sequence.
			"a tombstone at the floor", st.trust,
			BusTrustRecord{BusID: psOriginBus, ConfigSeq: 5, State: PeerRecordRemoved, UpdatedAt: at}, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st.mu.Lock()
			err := st.applyLocked(tc.table, tc.rec, "test")
			st.mu.Unlock()
			if got := errors.Is(err, ErrPeerWithdrawn); got != tc.refused {
				t.Fatalf("errors.Is(err, ErrPeerWithdrawn) = %v, want %v (err %v)", got, tc.refused, err)
			}
		})
	}
}

// TestPeerWithdrawalFloorNeverPersistsWhatItCannotReadBack is a REGRESSION test
// for the security re-verification's P2-a, whose outcome was the worst failure
// available in this file: a bus that never boots again, with a printed remedy
// that LOOPS.
//
// The write side validated against maxConfigSeq while the read side refused at
// maxPlausiblePeerWithdrawalSeq, so an acknowledged withdrawal could persist a
// file the next start refuses. Following that refusal's remedy — move it aside,
// restart — had reconcileWithdrawalFloor re-derive the same out-of-range value
// from the tombstone still in the log and write the identical unreadable file.
//
// The rule the two arms pin: THIS BINARY NEVER WRITES A FLOOR FILE IT WOULD
// REFUSE TO READ, whether the sequence comes from a mint or from the log.
func TestPeerWithdrawalFloorNeverPersistsWhatItCannotReadBack(t *testing.T) {
	t.Run("the encoder refuses an out-of-range sequence", func(t *testing.T) {
		for _, seq := range []uint64{maxPlausiblePeerWithdrawalSeq, maxPlausiblePeerWithdrawalSeq + 1, maxConfigSeq} {
			_, err := encodePeerWithdrawalFloors([]peerWithdrawalEntry{{table: trustTableToken, busID: psOriginBus, seq: seq}})
			if !errors.Is(err, ErrPeerWithdrawalFloorCorrupt) {
				t.Errorf("encoding a floor of %d returned %v, want a refusal; the reader would reject this file and the bus would never start again", seq, err)
			}
		}
	})

	t.Run("a withdrawal minted above the bound is refused and the bus still starts", func(t *testing.T) {
		// Seed config_seq just below the bound, exactly as the gate did: this is
		// the value TestPeerWithdrawalFloorRefusesAnImplausibleSequence requires
		// to be ADOPTED, so the two rules meet here.
		dir := t.TempDir()
		body := "trust bus-ps-seed " + strconv.FormatUint(maxPlausiblePeerWithdrawalSeq-1, 10) + "\n"
		h := sha256.Sum256([]byte(body))
		data := peerWithdrawalFloorMagic + " v6 sha256=" + hex.EncodeToString(h[:]) + "\n" + body
		if err := os.WriteFile(filepath.Join(dir, PeerWithdrawalFloorFileName), []byte(data), 0o600); err != nil {
			t.Fatalf("planting: %v", err)
		}

		st, lg := psOpenStore(t, dir, nil, nil)
		if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
			t.Fatalf("PutTrust: %v", err)
		}
		// The withdrawal would mint at or above the bound. It must be REFUSED,
		// loudly, rather than acknowledged and persisted unreadably.
		err := func() error { _, e := st.RemoveTrust(psOriginBus); return e }()
		if !errors.Is(err, ErrPeerWithdrawalSeqTooHigh) {
			t.Fatalf("RemoveTrust returned %v, want ErrPeerWithdrawalSeqTooHigh; an acknowledged withdrawal here writes a floor file this binary cannot read back", err)
		}
		// THE DIAGNOSIS MUST NOT BLAME THE FILE. This case used to wrap
		// ErrPeerWithdrawalFloorCorrupt, whose remedy is "move it aside and
		// restart" — which would DELETE a healthy floor and permanently lose
		// every revocation whose tombstone had been swept, while still not
		// letting the operator withdraw. A security-gate finding, and the reason
		// the sentinel is separate.
		if errors.Is(err, ErrPeerWithdrawalFloorCorrupt) {
			t.Errorf("the refusal claims the floor FILE is corrupt; it is intact, and that error's remedy destroys it: %v", err)
		}
		if !strings.Contains(err.Error(), "THE FLOOR FILE IS NOT CORRUPT") {
			t.Errorf("the refusal does not warn the operator off the wrong remedy: %v", err)
		}
		if err := lg.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// THE BUS STILL STARTS. That is the property the whole finding is about.
		st2, lg2 := psOpenStore(t, dir, nil, nil)
		defer func() { _ = lg2.Close() }()
		if pins := st2.PinnedKeys(psOriginBus); len(pins) != 1 {
			t.Errorf("after the refused withdrawal the pin should still stand (refusal is fail-closed on the FLOOR, not on the pin); got %x", pins)
		}
	})

	t.Run("reconcile rejects an out-of-range sequence from the log by name", func(t *testing.T) {
		// This arm asserts the LOG LINE as well as the outcome, deliberately, and
		// the reason is worth stating because a log-substring assertion is
		// normally a weak test.
		//
		// The reconcile-side bound is DEFENCE IN DEPTH behind the encoder's: with
		// it removed, reconcile still writes nothing, because encoding the
		// out-of-range entry fails anyway. The two are distinguishable only by
		// what the operator is told — "this record's sequence is implausible" (a
		// diagnosis) versus "the floor could not be repaired" (a symptom, with the
		// cause left to be guessed). That difference IS the value of the bound, so
		// it is what the test pins; without it the bound is untestable and a
		// future edit would delete it with the suite green.
		dir := t.TempDir()
		sink := &psLogSink{}
		st, lg := psOpenStore(t, dir, func(o *PeerStoreOptions) {
			o.Logger = logging.New(sink, logging.LevelDebug)
		}, nil)
		defer func() { _ = lg.Close() }()

		// A tombstone carrying a sequence no mint could produce — a forged frame,
		// or a record written by a future binary. Before the fix, reconcile copied
		// it verbatim into the floor and made the directory unstartable, with a
		// printed remedy that rebuilt the same unreadable file.
		if err := st.Apply(psCommitted(t, BusTrustRecord{
			BusID: psOriginBus, ConfigSeq: 1 << 40, State: PeerRecordRemoved,
			UpdatedAt: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
		}, 10)); err != nil {
			t.Fatalf("Apply returned %v; it must never return an error", err)
		}

		if _, err := os.Stat(filepath.Join(dir, PeerWithdrawalFloorFileName)); !os.IsNotExist(err) {
			floors := psFloors(t, dir)
			t.Fatalf("reconcile wrote a floor from an out-of-range log record (%+v); the next start would refuse it and the remedy would rebuild it", floors)
		}
		if out := sink.String(); !strings.Contains(out, "REFUSING to record a withdrawal floor from a log record whose config_seq is implausibly high") {
			t.Errorf("the out-of-range record was not diagnosed by name; the operator gets a symptom instead of a cause. Log was:\n%s", out)
		}
	})
}

// TestPeerWithdrawalFloorSurvivesConcurrentWriters is a REGRESSION test for the
// security re-verification's P2-b.
//
// recordWithdrawal releases s.mu across the file write and rebuilds the WHOLE
// file from the in-memory mirror. Its safety was argued from writeMu — but
// reconcileWithdrawalFloor is a second caller that holds no writeMu and CANNOT
// (it runs from Apply, which write() reaches while already holding it). Two
// callers could therefore interleave snapshot-then-write, and the second could
// write a snapshot taken before the first's entry existed, DROPPING a floor its
// caller had already been told was recorded.
//
// The gate reproduced exactly this: a concurrent Remove and Apply left the route
// floor missing from disk although Remove returned success.
func TestPeerWithdrawalFloorSurvivesConcurrentWriters(t *testing.T) {
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg.Close() }()

	if _, err := st.Put(PeerConfig{BusID: psRemoteBus, BaseURL: psURLGen1}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := st.Remove(psRemoteBus); err != nil {
			t.Errorf("Remove: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		// A withdrawal arriving through the APPLY path, which is the caller that
		// cannot hold writeMu.
		st.Apply(psCommitted(t, BusTrustRecord{
			BusID: psOriginBus, ConfigSeq: 500, State: PeerRecordRemoved, UpdatedAt: at,
		}, 20))
	}()
	wg.Wait()

	// BOTH floors must be on disk. The bug dropped whichever lost the race, while
	// its caller had been told the withdrawal was recorded.
	floors := psFloors(t, dir)
	if got := floors[trustTableToken][psOriginBus]; got != 500 {
		t.Errorf("the trust floor for %s is %d, want 500; a concurrent writer dropped it (floors %+v)", psOriginBus, got, floors)
	}
	if got := floors[routeTableToken][psRemoteBus]; got == 0 {
		t.Errorf("the route floor for %s is missing although Remove returned success; a concurrent writer dropped it (floors %+v)", psRemoteBus, floors)
	}
}

// TestPeerStoreNeverFloorsItsOwnBusID is a REGRESSION test for the security
// gate's round-3 P3.
//
// reconcileWithdrawalFloor runs on records applyLocked REFUSED, which is
// deliberate — a record refused by the in-memory table can still be a
// withdrawal whose floor must be recorded. But a record naming OUR OWN bus is
// refused on every path as a self-peer, and nothing may legitimately produce
// one. Flooring it wrote a permanent row for this bus's own id: an entry no
// operator action can ever explain, remove, or have caused.
func TestPeerStoreNeverFloorsItsOwnBusID(t *testing.T) {
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg.Close() }()

	if err := st.Apply(psCommitted(t, BusTrustRecord{
		BusID: psLocalBus, ConfigSeq: 7, State: PeerRecordRemoved,
		UpdatedAt: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
	}, 30)); err != nil {
		t.Fatalf("Apply returned %v; it must never return an error", err)
	}

	if _, err := os.Stat(filepath.Join(dir, PeerWithdrawalFloorFileName)); !os.IsNotExist(err) {
		t.Fatalf("a withdrawal naming our OWN bus id was floored (%+v); a self-peer is refused on every other path and must not become a permanent row nothing can explain", psFloors(t, dir))
	}
}

// TestPeerWithdrawalFloorAdoptsOnlyAfterAWriteThatLANDED is a REGRESSION test
// for the reviewer gate's round-2 P1-A: moving `t.withdrawnAt[folded] = seq`
// from AFTER atomicReplacePeerWithdrawalFloor to before it left the entire
// suite green, while the code documents "Memory NEVER claims more than disk".
//
// The consequence is RELAY-34's own fail-open, reached by an ordinary operator
// mistake rather than by damage. If memory adopts a floor the disk does not
// hold:
//
//   - the failed RemoveTrust has nonetheless made the pin invisible in memory,
//     so every RETRY reports ErrUnknownPeer and the operator can never complete
//     the revocation without restarting;
//   - and after the restart the floor is absent, so THE REVOKED PIN COMES BACK.
//
// The discriminator is a directory the process cannot write, which is the
// cheapest honest way to make the floor write fail without stubbing it.
func TestPeerWithdrawalFloorAdoptsOnlyAfterAWriteThatLANDED(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not restrain root, so the floor write cannot be made to fail this way")
	}
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg.Close() }()
	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := st.RemoveTrust(psOriginBus); err == nil {
		_ = os.Chmod(dir, 0o700)
		t.Fatalf("RemoveTrust succeeded on a read-only data directory")
	}

	// NOTHING MAY HAVE BEEN ADOPTED. The pin must still be served, because the
	// revocation demonstrably did not become durable and the operator was told so.
	if pins := st.PinnedKeys(psOriginBus); len(pins) != 1 {
		_ = os.Chmod(dir, 0o700)
		t.Fatalf("after a FAILED floor write the pin reads as %x; memory adopted a withdrawal disk does not hold, so a restart would bring the key back and every retry would report ErrUnknownPeer", pins)
	}

	// AND THE RETRY MUST WORK once the directory is writable again — the failure
	// left the store in a state the operator can still act on.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	if _, err := st.RemoveTrust(psOriginBus); err != nil {
		t.Fatalf("retrying the withdrawal after the directory became writable: %v", err)
	}
	if got := psFloors(t, dir)[trustTableToken][psOriginBus]; got == 0 {
		t.Fatalf("the retried withdrawal recorded no floor")
	}
	if pins := st.PinnedKeys(psOriginBus); len(pins) != 0 {
		t.Fatalf("after the successful retry the pin is still served: %x", pins)
	}
}

// TestPeerStoreCapCountsASlotTheBusAlreadyHolds pins the reviewer gate's own
// round-1 P2 fix, which was correct but undefended: reverting `slotHeld` to
// `known` left the suite green.
//
// A floor-hidden record still OCCUPIES its slot, so gating the capacity check on
// `known` refused an operator RECONFIGURING a bus the table already holds, at
// exactly MaxPeers, when no new slot was needed at all.
func TestPeerStoreCapCountsASlotTheBusAlreadyHolds(t *testing.T) {
	dir := t.TempDir()
	// THE SLOT MUST BE HELD BY A FLOOR-HIDDEN *ACTIVE* RECORD, and getting that
	// state right is the whole test. An earlier version held it with a TOMBSTONE
	// and therefore tested nothing: busTable.withdrawn returns false for any
	// non-Active record, so `known` was never forced false, the `if !known`
	// branch was never entered, and the guard under test was never reached —
	// slotHeld and known are indistinguishable in that state. The reviewer gate
	// caught it, and caught me claiming the mutant died when it had not.
	//
	// The state that does exercise it is the one
	// TestPeerStoreHidesAPinWhoseFloorLandedButWhoseTombstoneDidNot builds: the
	// withdrawal's FLOOR lands and its log entry does not, so the table still
	// holds a live ACTIVE record that every reader must treat as absent.
	fail := &psFailWriteOfKind{kind: BusTrustRecordKind, state: PeerRecordRemoved}
	st, lg := psOpenStore(t, dir, func(o *PeerStoreOptions) { o.MaxPeers = 1 }, func(inner PeerDurableLog) PeerDurableLog {
		fail.inner = inner
		return fail
	})
	defer func() { _ = lg.Close() }()

	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	if _, err := st.RemoveTrust(psOriginBus); err == nil {
		t.Fatalf("RemoveTrust succeeded; the injected failure did not fire, so no floor-hidden ACTIVE record exists and this test would prove nothing")
	}
	if pins := st.PinnedKeys(psOriginBus); len(pins) != 0 {
		t.Fatalf("the floor did not land: PinnedKeys = %x", pins)
	}
	// The table is at MaxPeers=1 and the slot is held by this very bus, whose
	// record is now floor-hidden. Re-pinning it needs no new slot.
	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(2)}}); err != nil {
		t.Fatalf("re-pinning a bus whose slot this table already holds was refused: %v; the cap counted a slot that did not need allocating", err)
	}
	if pins := st.PinnedKeys(psOriginBus); len(pins) != 1 || !bytes.Equal(pins[0], psKey(2)) {
		t.Fatalf("PinnedKeys = %x, want the re-pinned key", pins)
	}
	// A DIFFERENT bus must still be refused: the cap is real.
	if _, err := st.PutTrust(BusTrust{BusID: psRemoteBus, SigningKeys: []ed25519.PublicKey{psKey(3)}}); !errors.Is(err, ErrTooManyPeers) {
		t.Fatalf("a NEW bus at the cap returned %v, want ErrTooManyPeers; the cap has been disabled rather than corrected", err)
	}
}

// TestPeerWithdrawalFloorEncoderRefusesUnpersistableEntries covers the encoder's
// own validation, which had ZERO test references although its comment argues the
// checks are what stop a data directory being permanently stranded (a file it
// writes but cannot read back is never regenerated). All three mutants survived.
func TestPeerWithdrawalFloorEncoderRefusesUnpersistableEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []peerWithdrawalEntry
	}{
		{"an invalid bus id", []peerWithdrawalEntry{{table: trustTableToken, busID: "bus.with.dots", seq: 1}}},
		{"a bus id carrying a space", []peerWithdrawalEntry{{table: trustTableToken, busID: "bus one", seq: 1}}},
		{"a bus id carrying a newline", []peerWithdrawalEntry{{table: trustTableToken, busID: "bus\ntrust bus-x 9", seq: 1}}},
		{"an UNFOLDED bus id", []peerWithdrawalEntry{{table: trustTableToken, busID: "BUS-PS-ORIGIN", seq: 1}}},
		{"an unknown table token", []peerWithdrawalEntry{{table: "pins", busID: psOriginBus, seq: 1}}},
		{"a zero sequence", []peerWithdrawalEntry{{table: trustTableToken, busID: psOriginBus, seq: 0}}},
		{
			"a duplicated (table, bus)",
			[]peerWithdrawalEntry{
				{table: trustTableToken, busID: psOriginBus, seq: 7},
				{table: trustTableToken, busID: psOriginBus, seq: 2},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := encodePeerWithdrawalFloors(tc.entries); !errors.Is(err, ErrPeerWithdrawalFloorCorrupt) {
				t.Fatalf("encoding returned %v, want a refusal; a file written with this entry could never be read back, and a floor file that cannot be read back is never regenerated", err)
			}
		})
	}
}

// TestPeerStoreNeverFloorsACaseConfusable is the other half of the reviewer's
// reconcile finding, alongside TestPeerStoreNeverFloorsItsOwnBusID.
//
// Two bus ids differing only by ASCII case are two DIFFERENT buses downstream,
// and the table refuses the confusable. Flooring it anyway would durably un-pin
// the LEGITIMATE bus — a revocation nobody performed.
func TestPeerStoreNeverFloorsACaseConfusable(t *testing.T) {
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	st, lg := psOpenStore(t, dir, nil, nil)
	defer func() { _ = lg.Close() }()

	if _, err := st.PutTrust(BusTrust{BusID: psOriginBus, SigningKeys: []ed25519.PublicKey{psKey(1)}}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	confusable := strings.ToUpper(psOriginBus)
	if confusable == psOriginBus {
		t.Fatalf("fixture error: %q has no case variant", psOriginBus)
	}
	if err := st.Apply(psCommitted(t, BusTrustRecord{
		BusID: confusable, ConfigSeq: 900, State: PeerRecordRemoved, UpdatedAt: at,
	}, 40)); err != nil {
		t.Fatalf("Apply returned %v; it must never return an error", err)
	}

	if pins := st.PinnedKeys(psOriginBus); len(pins) != 1 {
		t.Fatalf("a withdrawal naming an ASCII-case CONFUSABLE of %s un-pinned it (pins %x); that is a revocation nobody performed", psOriginBus, pins)
	}
	if _, err := os.Stat(filepath.Join(dir, PeerWithdrawalFloorFileName)); !os.IsNotExist(err) {
		t.Fatalf("a case-confusable withdrawal was floored: %+v", psFloors(t, dir))
	}
}
