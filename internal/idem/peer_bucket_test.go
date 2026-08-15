package idem_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
)

func peerStore(t *testing.T, local string, max int, now time.Time) *idem.Store {
	t.Helper()
	s, err := idem.NewStoreForBus(local, idem.StoreOptions{MaxEntries: max, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewStoreForBus(%q): %v", local, err)
	}
	return s
}

func peerRecord(agent string, n int, at time.Time) idem.Record {
	return idem.Record{
		Agent: agent, Op: idem.OpRelay, Key: fmt.Sprintf("peer-key-%d", n),
		Fingerprint: idem.ComputeFingerprint([]byte(fmt.Sprintf("payload-%d", n))),
		CommittedAt: at,
	}
}

func TestPeerBucket32766InventedLabelsCountAsOnePeer(t *testing.T) {
	at := time.Unix(1700000000, 0)
	s := peerStore(t, "bus-local", idem.MaxEntries, at)
	for i := 0; i < 2; i++ {
		if err := s.Remember(peerRecord("bus-local.victim-1", i, at)); err != nil {
			t.Fatalf("Remember victim's pre-existing key %d: %v", i, err)
		}
	}
	for i := 1; i <= 32766; i++ {
		if err := s.Remember(peerRecord(fmt.Sprintf("bus-peer.fake-%d", i), i+2, at)); err != nil {
			t.Fatalf("Remember invented label %d: %v", i, err)
		}
	}
	st := s.Stats()
	if idem.PressureLine != 32768 {
		t.Fatalf("PressureLine=%d, want literal demonstrated boundary 32768", idem.PressureLine)
	}
	if st.Count != 32768 || st.Agents != 2 || st.Share != 21845 || !st.UnderPressure {
		t.Fatalf("Stats=%+v; want literal boundary Count=32768, Agents=2, Share=21845, UnderPressure=true", st)
	}
	if err := s.Remember(peerRecord("BUS-PEER.more-1", 32769, at)); !errors.Is(err, idem.ErrAgentQuota) {
		t.Fatalf("flooding peer B's next key: err=%v, want ErrAgentQuota", err)
	}
	if got := s.Stats().Count; got != 32768 {
		t.Fatalf("B refusal changed Count to %d, want 32768: refusal must neither insert nor drain retained keys", got)
	}
	if err := s.Remember(peerRecord("bus-honest-c.existing-1", 32770, at)); err != nil {
		t.Fatalf("honest foreign peer C's existing traffic was refused during B's flood: %v", err)
	}
	if err := s.Remember(peerRecord("bus-honest-c.existing-1", 32771, at)); err != nil {
		t.Fatalf("honest foreign peer C could not continue service after B's flood: %v", err)
	}
	if err := s.Remember(peerRecord("bus-local.victim-1", 32772, at)); err != nil {
		t.Fatalf("victim's third key was locked out at the measured attack boundary: %v", err)
	}
	if got := s.Stats().Count; got != 32771 {
		t.Fatalf("triad Count=%d, want 32771: B refused, C added two, same local victim added one, and no record drained", got)
	}
}

func TestPeerBucketRecoveryParityAt32768Boundary(t *testing.T) {
	at := time.Unix(1700000000, 0)
	live := peerStore(t, "bus-local", idem.MaxEntries, at)
	recovered := peerStore(t, "bus-local", idem.MaxEntries, at)
	records := make([]idem.Record, 0, idem.PressureLine)
	records = append(records,
		peerRecord("bus-local.victim-1", 0, at),
		peerRecord("bus-local.victim-1", 1, at),
	)
	for i := 1; i <= 32766; i++ {
		records = append(records, peerRecord(fmt.Sprintf("BUS-PEER.fake-%d", i), i+2, at))
	}
	for i, r := range records {
		if err := live.Remember(r); err != nil {
			t.Fatalf("live Remember %d: %v", i, err)
		}
		if err := recovered.Recover(r); err != nil {
			t.Fatalf("Recover %d: %v", i, err)
		}
	}
	liveStats, recoveredStats := live.Stats(), recovered.Stats()
	if liveStats.Count != 32768 || liveStats.Agents != 2 || liveStats.Share != 21845 || !liveStats.UnderPressure || recoveredStats.Count != liveStats.Count || recoveredStats.Agents != liveStats.Agents || recoveredStats.Share != liveStats.Share || recoveredStats.UnderPressure != liveStats.UnderPressure {
		t.Fatalf("boundary parity mismatch: live=%+v recovered=%+v", liveStats, recoveredStats)
	}
	if err := recovered.Remember(peerRecord("bus-peer.more-1", 32769, at)); !errors.Is(err, idem.ErrAgentQuota) {
		t.Fatalf("post-recovery flooding peer B's next key: err=%v, want ErrAgentQuota", err)
	}
	if got := recovered.Stats().Count; got != 32768 {
		t.Fatalf("post-recovery B refusal changed Count to %d, want 32768", got)
	}
	cExisting := peerRecord("bus-honest-c.existing-1", 32770, at)
	if err := recovered.Recover(cExisting); err != nil {
		t.Fatalf("recover honest peer C's existing traffic: %v", err)
	}
	if err := recovered.Remember(peerRecord("bus-honest-c.existing-1", 32771, at)); err != nil {
		t.Fatalf("honest peer C could not continue service after recovery: %v", err)
	}
	if err := recovered.Remember(peerRecord("bus-local.victim-1", 32772, at)); err != nil {
		t.Fatalf("victim's same-holder third key was locked out after recovery: %v", err)
	}
	if got := recovered.Stats().Count; got != 32771 {
		t.Fatalf("post-recovery triad Count=%d, want 32771 with no drained record", got)
	}
}

func TestPeerBucketQuotaSeparatesPeersWithoutCollateralRefusal(t *testing.T) {
	at := time.Unix(1700000000, 0)
	s := peerStore(t, "bus-local", 16, at)
	for i := 1; i <= 8; i++ {
		if err := s.Remember(peerRecord(fmt.Sprintf("bus-hog.fake-%d", i), i, at)); err != nil {
			t.Fatalf("filling pressure line: %v", err)
		}
	}
	if err := s.Remember(peerRecord("BUS-HOG.more-1", 9, at)); !errors.Is(err, idem.ErrAgentQuota) {
		t.Fatalf("same peer under alternate case: err=%v, want ErrAgentQuota", err)
	} else if got := err.Error(); got == "" || !containsAll(got, "foreign peer bus", "bus-hog", "that peer bus's keys") {
		t.Fatalf("foreign quota diagnostic %q does not accurately identify its peer-bus bucket", got)
	}
	if err := s.Remember(peerRecord("bus-other.real-1", 10, at)); err != nil {
		t.Fatalf("independent peer suffered collateral refusal: %v", err)
	}
	if got := s.Stats().Agents; got != 2 {
		t.Fatalf("Agents=%d, want two independently metered peers", got)
	}
}

func TestPeerBucketQuotaDiagnosticNamesLocalAgent(t *testing.T) {
	at := time.Unix(1700000000, 0)
	s := peerStore(t, "bus-local", 16, at)
	for i := 1; i <= 8; i++ {
		if err := s.Remember(peerRecord("bus-local.hog-1", i, at)); err != nil {
			t.Fatalf("filling pressure line: %v", err)
		}
	}
	err := s.Remember(peerRecord("bus-local.hog-1", 9, at))
	if !errors.Is(err, idem.ErrAgentQuota) || !errors.Is(err, idem.ErrCapacity) {
		t.Fatalf("local quota err=%v, want stable ErrAgentQuota and ErrCapacity sentinel matches", err)
	}
	if got := err.Error(); !containsAll(got, "local agent", "bus-local.hog-1", "that agent's keys") || strings.Contains(got, "foreign peer bus") {
		t.Fatalf("local quota diagnostic %q does not accurately identify its agent bucket", got)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}

func TestPeerBucketCanonicalizesBusCaseAndSeparatesPeers(t *testing.T) {
	at := time.Unix(1700000000, 0)
	s := peerStore(t, "BUS-LOCAL", 16, at)
	for i, agent := range []string{"BUS-PEER.one-1", "bus-peer.two-1", "bus-other.one-1", "BUS-LOCAL.local-1", "bus-local.other-1"} {
		if err := s.Remember(peerRecord(agent, i, at)); err != nil {
			t.Fatalf("Remember(%q): %v", agent, err)
		}
	}
	if got := s.Stats().Agents; got != 4 {
		t.Fatalf("Agents = %d, want peer case aliases coalesced, distinct peer separated, and two local agents distinct", got)
	}
}

func TestPeerBucketMalformedAgentFailsClosedLiveAndRecovery(t *testing.T) {
	at := time.Unix(1700000000, 0)
	for _, malformed := range []string{"", "unqualified-1", " bus-peer.a-1", "bus-peer.a-1 "} {
		t.Run(fmt.Sprintf("%q", malformed), func(t *testing.T) {
			for _, recover := range []bool{false, true} {
				s := peerStore(t, "bus-local", 16, at)
				r := peerRecord(malformed, 1, at)
				var err error
				if recover {
					err = s.Recover(r)
				} else {
					err = s.Remember(r)
				}
				if !errors.Is(err, idem.ErrInvalidRecord) {
					t.Fatalf("recover=%v err=%v, want ErrInvalidRecord", recover, err)
				}
				if got := s.Stats().Count; got != 0 {
					t.Fatalf("recover=%v retained %d malformed records", recover, got)
				}
			}
		})
	}
}

func TestPeerBucketRecoveryRebuildsSameDenominator(t *testing.T) {
	at := time.Unix(1700000000, 0)
	records := []idem.Record{
		peerRecord("BUS-PEER.one-1", 1, at),
		peerRecord("bus-peer.two-1", 2, at),
		peerRecord("bus-other.one-1", 3, at),
	}
	live := peerStore(t, "bus-local", 16, at)
	recovered := peerStore(t, "bus-local", 16, at)
	for _, r := range records {
		if err := live.Remember(r); err != nil {
			t.Fatal(err)
		}
		if err := recovered.Recover(r); err != nil {
			t.Fatal(err)
		}
	}
	if live.Stats().Agents != 2 || recovered.Stats().Agents != live.Stats().Agents {
		t.Fatalf("live agents=%d recovered agents=%d, want identical two-peer denominator", live.Stats().Agents, recovered.Stats().Agents)
	}
}

func TestPeerBucketExpiryDecrementsCanonicalBucket(t *testing.T) {
	now := time.Unix(1700000000, 0)
	s, err := idem.NewStoreForBus("bus-local", idem.StoreOptions{
		MaxEntries: 16,
		Window:     time.Minute,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, agent := range []string{"BUS-PEER.one-1", "bus-peer.two-1"} {
		if err := s.Remember(peerRecord(agent, i, now)); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Stats().Agents; got != 1 {
		t.Fatalf("before expiry Agents=%d, want one canonical peer bucket", got)
	}
	now = now.Add(time.Minute + time.Nanosecond)
	st := s.Stats()
	if st.Count != 0 || st.Agents != 0 || st.Expired != 2 {
		t.Fatalf("after expiry Stats=%+v, want empty table, zero buckets and two visible expirations", st)
	}
}

func TestNewStoreForBusRequiresValidLocalBusID(t *testing.T) {
	for _, id := range []string{"", "bad.bus", " bus-local"} {
		if _, err := idem.NewStoreForBus(id, idem.StoreOptions{}); err == nil {
			t.Fatalf("NewStoreForBus(%q) succeeded, want fail-closed validation", id)
		}
	}
}
