package main

// RELAY-24 — the federation composition root.
//
// Every test here drives a DECISION FUNCTION with an explicit peerBusID rather
// than a context, which is the shape the wiring uses precisely so these rules
// are provable without standing up mutual TLS. The context adapters are covered
// by the one route-composition test, which proves the surface is mounted and
// gated at all.
//
// NO TEST IN THIS FILE NAMES A PEER ROUTE PATH. internal/relay/guards_test.go
// forbids it outside the one reviewed mount file, so the route assertions below
// work off the values httpapi.Server.PeerRoutes() returns.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	wiringLocalBus = "wirebus"
	wiringPeerBus  = "peerbus"
	wiringThirdBus = "thirdbus"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// stubIngest is a LocalIngest that records what it was asked and answers what
// the test told it to. It exists so the BINDING and METERING rules can be driven
// without a durable hub; the hub-backed adapter has its own test below, over a
// real hub, because that is the seam idem.Outcome crosses.
type stubIngest struct {
	mu       sync.Mutex
	enrolled map[string]bool
	outcome  idem.Outcome
	err      error
	calls    int
	release  chan struct{}
}

func (s *stubIngest) Enrolled(agentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enrolled[agentID]
}

func (s *stubIngest) AcceptRelayed(ctx context.Context, m relay.RelayedMessage) (relay.LocalAcceptance, error) {
	s.mu.Lock()
	s.calls++
	// ONLY THE FIRST call parks on the gate. A gate that held every caller would
	// make the concurrency test DEADLOCK rather than fail when the cap is
	// removed, and a test that hangs on a broken guard is a test whose verdict is
	// "timeout" — indistinguishable from an unrelated hang, and 10 minutes slower
	// than an assertion.
	first := s.calls == 1
	out, err, gate := s.outcome, s.err, s.release
	s.mu.Unlock()
	if gate != nil && first {
		<-gate
	}
	if err != nil {
		return relay.LocalAcceptance{Outcome: out}, err
	}
	return relay.LocalAcceptance{LocalMessageID: wiringLocalBus + "-1", Outcome: out}, nil
}

func (s *stubIngest) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newWiringPeerStore builds a peer store over a scratch directory. It is
// READ-ONLY (no Durable), exactly as the server builds it.
func newWiringPeerStore(t *testing.T) *relay.PeerStore {
	t.Helper()
	store, err := relay.NewPeerStore(relay.PeerStoreOptions{BusID: wiringLocalBus, Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("relay.NewPeerStore: %v", err)
	}
	return store
}

// newWiringRegistry builds the routing table newFederation now takes rather than
// constructs (RELAY-24-BLOCKER-EGRESS). Production shares ONE registry between
// the ingress assembled here and the egress forwarder built before the hub; a
// test that only exercises the ingress still has to supply one.
func newWiringRegistry(t *testing.T) *relay.Registry {
	t.Helper()
	reg, err := relay.NewRegistry(relay.RegistryOptions{BusID: wiringLocalBus})
	if err != nil {
		t.Fatalf("relay.NewRegistry: %v", err)
	}
	return reg
}

// newWiringFederation assembles a federation with a stub local bus. share and
// concurrent are the admission bounds; 0 means the production default.
func newWiringFederation(t *testing.T, local relay.LocalIngest, share, concurrent int) *federation {
	t.Helper()
	fed, err := newFederation(federationOptions{
		BusID:                wiringLocalBus,
		Registry:             newWiringRegistry(t),
		Local:                local,
		Peers:                newWiringPeerStore(t),
		LocalAgents:          func() []string { return nil },
		AppliedKeyShare:      share,
		MaxConcurrentPerPeer: concurrent,
		// The quota only REFUSES under pressure (it is a share, not a rate), so a
		// test that wants to demonstrate the bound must say the table is contended.
		// Passing it explicitly also keeps these tests honest about which of the
		// two conditions they are exercising.
		UnderPressure: func() bool { return true },
	})
	if err != nil {
		t.Fatalf("newFederation: %v", err)
	}
	return fed
}

// relayedTo builds a minimally valid relayed message addressed to `to`, arriving
// over `path`.
func relayedTo(t *testing.T, to string, path ...string) relay.RelayedMessage {
	t.Helper()
	sender, err := ids.AgentID(wiringPeerBus, "alpha", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	return relay.RelayedMessage{
		OriginBus:       wiringPeerBus,
		OriginMessageID: wiringPeerBus + "-1",
		Sender:          sender,
		Recipients:      []string{to},
		BusPath:         path,
		Body:            []byte("hello"),
	}
}

// localAgent is a fully-qualified agent id in THIS bus's namespace.
func localAgent(t *testing.T, name string) string {
	t.Helper()
	id, err := ids.AgentID(wiringLocalBus, name, 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// 1. The surface composes, is mounted, and is gated
// ---------------------------------------------------------------------------

// TestRelayWiringComposesRoutesWhenPeersConfigured is this task's proof command.
//
// It asserts the three properties a composition root can get wrong silently:
// the surface is COMPLETE (a partial one registers nothing), it is paired with
// the RESOLVER (without which the same three routes would answer 403 to
// everyone), and every registered route is GATED — an anonymous caller is
// refused rather than served.
func TestRelayWiringComposesRoutesWhenPeersConfigured(t *testing.T) {
	store := newWiringPeerStore(t)
	fed, err := newFederation(federationOptions{
		BusID:       wiringLocalBus,
		Registry:    newWiringRegistry(t),
		Local:       &stubIngest{},
		Peers:       store,
		LocalAgents: func() []string { return nil },
	})
	if err != nil {
		t.Fatalf("newFederation: %v", err)
	}
	surface := fed.Surface()
	if surface == nil {
		t.Fatal("newFederation returned no peer surface")
	}
	// EVERY FIELD IS REQUIRED, and a nil one makes httpapi register nothing at
	// all. Naming them here means a future field added to PeerSurface and left
	// unset by newFederation fails HERE rather than as an unexplained 404.
	if surface.Enroll == nil || surface.Relay == nil || surface.Roster == nil ||
		surface.Registry == nil || surface.Trust == nil {
		t.Fatalf("incomplete PeerSurface: enroll=%v relay=%v roster=%v registry=%v trust=%v",
			surface.Enroll != nil, surface.Relay != nil, surface.Roster != nil,
			surface.Registry != nil, surface.Trust != nil)
	}

	newServer := func(peer *httpapi.PeerSurface, principals httpapi.InboundPeerPrincipals) *httpapi.Server {
		return httpapi.New(httpapi.Options{
			Identity:       ids.BusIdentity(wiringLocalBus),
			Logger:         logging.New(io.Discard, logging.LevelError),
			Peer:           peer,
			PeerPrincipals: principals,
		})
	}

	srv := newServer(surface, store)
	routes := srv.PeerRoutes()
	if len(routes) != 3 {
		t.Fatalf("PeerRoutes() = %v (%d routes), want 3 registered peer routes", routes, len(routes))
	}

	// GATED, not merely present. An anonymous caller over plain HTTP presents no
	// client certificate, so the peer principal gate must refuse — 403, never a
	// 200 and never the 401 an ungated-but-bearer-required path would give.
	for _, route := range routes {
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("anonymous POST %s: status %d, want %d — a registered peer route must be refused by the certificate gate",
				route, rec.Code, http.StatusForbidden)
		}
	}

	// WITHOUT THE RESOLVER, NOTHING IS REGISTERED. This is the pairing main.go
	// makes; supplying one without the other is the mis-wiring that would put
	// three routes on the wire that no peer can ever pass.
	if got := newServer(surface, nil).PeerRoutes(); len(got) != 0 {
		t.Errorf("PeerRoutes() with no inbound resolver = %v, want none registered", got)
	}
	// AND WITHOUT THE SURFACE, NOTHING IS REGISTERED.
	if got := newServer(nil, store).PeerRoutes(); len(got) != 0 {
		t.Errorf("PeerRoutes() with no peer surface = %v, want none registered", got)
	}
}

// TestFederationRefusesAnIncompleteWiring pins the construction-time checks. A
// federation assembled with a piece missing must fail at startup, where the
// operator can read which side is broken, rather than at runtime as a peer's
// unexplained refusal.
func TestFederationRefusesAnIncompleteWiring(t *testing.T) {
	full := func() federationOptions {
		return federationOptions{
			BusID:       wiringLocalBus,
			Registry:    newWiringRegistry(t),
			Local:       &stubIngest{},
			Peers:       newWiringPeerStore(t),
			LocalAgents: func() []string { return nil },
		}
	}
	for _, tc := range []struct {
		name   string
		break_ func(*federationOptions)
	}{
		{"no local bus", func(o *federationOptions) { o.Local = nil }},
		// A nil registry is a construction error and NOT a "build one for me":
		// the caller owns the table because the egress half reads the same one.
		{"no registry", func(o *federationOptions) { o.Registry = nil }},
		{"no peer store", func(o *federationOptions) { o.Peers = nil }},
		{"no local roster", func(o *federationOptions) { o.LocalAgents = nil }},
		{"invalid bus id", func(o *federationOptions) { o.BusID = "" }},
		{"negative admission bound", func(o *federationOptions) { o.MaxConcurrentPerPeer = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := full()
			tc.break_(&opts)
			if _, err := newFederation(opts); err == nil {
				t.Fatal("newFederation accepted an incomplete wiring; a partial federation serves nobody and says nothing")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. idem.Outcome crosses the hub seam UNCOLLAPSED
// ---------------------------------------------------------------------------

// TestHubIngestReportsRetryOnASecondArrival is the zero-value trap, tested
// rather than argued.
//
// idem.Outcome's zero value is idem.OutcomeNew — the answer that RE-FORWARDS —
// so an adapter that fills in the message id and forgets the outcome reports
// every duplicate as new and amplifies exactly the traffic the applied-key table
// terminates. relay.Acceptor's empty-id guard cannot catch that shape. This one
// can: the SAME message is ingested twice through the real hub, and the second
// answer must be OutcomeRetry carrying the ORIGINAL local id.
func TestHubIngestReportsRetryOnASecondArrival(t *testing.T) {
	dir := t.TempDir()
	walLog, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = walLog.Close() })

	roster := hub.NewStaticRoster()
	to := localAgent(t, "bravo")
	roster.Add(hub.Agent{AgentID: to, Name: "bravo", EnrolledAt: time.Now().Add(-time.Hour)})

	path := walLog.Path()
	h, err := hub.Open(hub.Options{
		BusID:     wiringLocalBus,
		DataDir:   filepath.Dir(path),
		Durable:   walLog,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex: walLog.Recovered().NextIndex,
		Roster:    roster,
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}

	m := relayedTo(t, to, wiringPeerBus)
	m.TimestampUnixMilli = time.Now().UnixMilli()
	// The hub checks the signature's LENGTH and never verifies it — verification
	// happens in internal/relay, before the hub is reached — so a correctly sized
	// value is what this seam needs.
	m.Signature = make([]byte, signing.SignatureSize)

	ingest := hubIngest{h: h}
	first, err := ingest.AcceptRelayed(context.Background(), m)
	if err != nil {
		t.Fatalf("first AcceptRelayed: %v", err)
	}
	if first.Outcome != idem.OutcomeNew {
		t.Fatalf("first arrival reported %s, want %s", first.Outcome, idem.OutcomeNew)
	}
	if first.LocalMessageID == "" {
		t.Fatal("first arrival minted no local message id")
	}

	second, err := ingest.AcceptRelayed(context.Background(), m)
	if err != nil {
		t.Fatalf("second AcceptRelayed: %v", err)
	}
	if second.Outcome != idem.OutcomeRetry {
		t.Fatalf("SECOND arrival of one message reported %s, want %s — a duplicate reported as new is re-forwarded, which is the amplification loop the applied-key table exists to stop",
			second.Outcome, idem.OutcomeRetry)
	}
	if second.LocalMessageID != first.LocalMessageID {
		t.Fatalf("second arrival reported local id %q, want the ORIGINAL %q replayed verbatim (invariant 10)",
			second.LocalMessageID, first.LocalMessageID)
	}

	// And the acceptor above it must SEE the duplicate: this is the value that
	// decides whether the message travels any further.
	acceptor, err := relay.NewAcceptor(relay.AcceptOptions{BusID: wiringLocalBus, Local: ingest})
	if err != nil {
		t.Fatalf("relay.NewAcceptor: %v", err)
	}
	acc, err := acceptor.Accept(context.Background(), m)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !acc.Duplicate {
		t.Fatal("the acceptor reported a third arrival of one message as NEW; the outcome was collapsed somewhere on the hub seam")
	}
}

// ---------------------------------------------------------------------------
// 3. Every claimed id is bound to the connection (invariant 2)
// ---------------------------------------------------------------------------

// TestPeerEnrolBusIDIsBoundToTheConnection: peer B may not hand us bus C's
// roster. Without the binding, one request re-points every one of C's agents at
// B, because UpsertPeer installs a FULL roster by claimed bus id.
func TestPeerEnrolBusIDIsBoundToTheConnection(t *testing.T) {
	fed := newWiringFederation(t, &stubIngest{}, 0, 0)
	peerAgent, err := ids.AgentID(wiringPeerBus, "alpha", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	thirdAgent, err := ids.AgentID(wiringThirdBus, "alpha", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}

	// The peer asserting its OWN id is accepted and lands in the routing table.
	if err := fed.acceptPeerFrom(wiringPeerBus, relay.PeerRoster{BusID: wiringPeerBus, Agents: []string{peerAgent}}); err != nil {
		t.Fatalf("a peer asserting its own id was refused: %v", err)
	}
	if _, ok := fed.registry.Route(peerAgent); !ok {
		t.Fatal("the accepted peer's agent is not routable, so the handshake did not reach the registry")
	}

	// The SAME peer asserting a THIRD bus's id is refused, and nothing is
	// recorded for that bus.
	err = fed.acceptPeerFrom(wiringPeerBus, relay.PeerRoster{BusID: wiringThirdBus, Agents: []string{thirdAgent}})
	if err == nil {
		t.Fatal("peer B was allowed to enrol as bus C; one accepted request replaces C's entire roster with B's")
	}
	if _, ok := fed.registry.Route(thirdAgent); ok {
		t.Fatal("the refused claim still installed a route for the third bus")
	}

	// A spelling differing only by ASCII case is a confusable in the routing
	// subject and is refused rather than folded.
	//
	// IT IS ASSERTED ON A FRESH FEDERATION AND BY SENTINEL, both deliberately.
	// On the federation above, Registry.UpsertPeer would refuse the case variant
	// ITSELF as a collision with the peer already registered — so the test would
	// pass with the binding folded, proving nothing. A fresh registry has no
	// collision to hide behind, and ErrPeerRejected is the sentinel only THIS
	// check produces.
	fresh := newWiringFederation(t, &stubIngest{}, 0, 0)
	err = fresh.acceptPeerFrom(wiringPeerBus, relay.PeerRoster{BusID: strings.ToUpper(wiringPeerBus)})
	if err == nil {
		t.Fatal("a case-variant spelling of the peer's own id was accepted; a confusable in the routing subject must be refused, not folded")
	}
	if !errors.Is(err, relay.ErrPeerRejected) {
		t.Fatalf("the case-variant claim was refused as %v, not by the connection binding; the binding folded the case and something else caught it", err)
	}

	// NO PRINCIPAL AT ALL IS A REFUSAL, NOT A SKIP.
	if err := fed.acceptPeerFrom("", relay.PeerRoster{BusID: wiringPeerBus, Agents: []string{peerAgent}}); err == nil {
		t.Fatal("a handshake carrying no authenticated peer principal was accepted")
	}
}

// TestRosterUpdateBusIDIsBoundToTheConnection: the same rule on the ongoing
// sync, which is the surface that would be used to keep the hijack current.
func TestRosterUpdateBusIDIsBoundToTheConnection(t *testing.T) {
	fed := newWiringFederation(t, &stubIngest{}, 0, 0)
	peerAgent, err := ids.AgentID(wiringPeerBus, "alpha", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	thirdAgent, err := ids.AgentID(wiringThirdBus, "alpha", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	if err := fed.acceptPeerFrom(wiringPeerBus, relay.PeerRoster{BusID: wiringPeerBus}); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err := fed.acceptPeerFrom(wiringThirdBus, relay.PeerRoster{BusID: wiringThirdBus}); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	own := relay.RosterUpdate{BusID: wiringPeerBus, Version: 1, Added: []string{peerAgent}}
	if err := fed.applyRosterFrom(wiringPeerBus, own, "key-own", relay.RosterUpdateFingerprint(own)); err != nil {
		t.Fatalf("a peer updating its own roster was refused: %v", err)
	}

	foreign := relay.RosterUpdate{BusID: wiringThirdBus, Version: 1, Added: []string{thirdAgent}}
	if err := fed.applyRosterFrom(wiringPeerBus, foreign, "key-foreign", relay.RosterUpdateFingerprint(foreign)); err == nil {
		t.Fatal("peer B was allowed to push a roster update for bus C")
	}
	// Membership, not routability: Route resolves by the BUS HALF of an id and is
	// deliberately independent of roster freshness, so it says nothing about
	// whether this update landed. Knows is the roster question.
	if fed.registry.Knows(thirdAgent) {
		t.Fatal("the refused update still added the third bus's agent to that bus's roster")
	}
	if _, version, _ := fed.registry.Roster(wiringThirdBus); version != 0 {
		t.Fatalf("the refused update advanced bus C's roster version to %d", version)
	}
	if err := fed.applyRosterFrom("", own, "key-none", relay.RosterUpdateFingerprint(own)); err == nil {
		t.Fatal("a roster update carrying no authenticated peer principal was accepted")
	}
}

// TestRelayBusPathLastHopIsBoundToTheConnection: the ONE hop of the traversed
// path this bus can independently verify is verified. It does not make the rest
// of the path trustworthy — nothing can — but a peer may not stamp somebody
// else's id in the position it occupies.
func TestRelayBusPathLastHopIsBoundToTheConnection(t *testing.T) {
	to := localAgent(t, "bravo")
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeNew}
	fed := newWiringFederation(t, local, 0, 0)
	ctx := context.Background()

	if _, err := fed.acceptRelayFrom(ctx, wiringPeerBus, relayedTo(t, to, wiringPeerBus)); err != nil {
		t.Fatalf("a message whose last hop IS the sending peer was refused: %v", err)
	}
	if local.callCount() != 1 {
		t.Fatalf("the accepted message reached the local bus %d times, want 1", local.callCount())
	}

	// The peer claims the message came from a third bus directly, hiding its own
	// hop. Refused, and NOTHING is written.
	if _, err := fed.acceptRelayFrom(ctx, wiringPeerBus, relayedTo(t, to, wiringThirdBus)); err == nil {
		t.Fatal("a peer was allowed to send a path whose last hop is another bus")
	}
	// A multi-hop path whose last hop is the peer is fine; one whose last hop is
	// an intermediate is not.
	if _, err := fed.acceptRelayFrom(ctx, wiringPeerBus, relayedTo(t, to, wiringThirdBus, wiringPeerBus)); err != nil {
		t.Fatalf("a multi-hop path ending at the sending peer was refused: %v", err)
	}
	if _, err := fed.acceptRelayFrom(ctx, wiringPeerBus, relayedTo(t, to, wiringPeerBus, wiringThirdBus)); err == nil {
		t.Fatal("a path whose last hop is not the sending peer was accepted")
	}
	// NO PRINCIPAL AT ALL. It is refused — though note WHY, because the obvious
	// reading is wrong: an empty principal can never equal a non-empty last hop,
	// so the ordinary mismatch arm would refuse it even with the empty-principal
	// checks removed. What the dedicated check actually buys is the SECOND
	// assertion: an unauthenticated caller must not be METERED, because bucketing
	// it would file every such request under one shared "" principal — a bucket
	// nobody owns, which both hides them from the per-peer bound and lets any one
	// of them exhaust the in-flight slots of all the others.
	if _, err := fed.acceptRelayFrom(ctx, "", relayedTo(t, to, wiringPeerBus)); err == nil {
		t.Fatal("a relayed message carrying no authenticated peer principal was accepted")
	}
	fed.admission.mu.Lock()
	_, metered := fed.admission.buckets[""]
	fed.admission.mu.Unlock()
	if metered {
		t.Fatal("a request with no authenticated peer principal was given a meter bucket of its own; every unauthenticated caller would share one bucket that nobody owns")
	}
	if local.callCount() != 2 {
		t.Fatalf("the local bus was reached %d times; every refusal above must write NOTHING", local.callCount())
	}
}

// ---------------------------------------------------------------------------
// 4. The relay path is metered by the AUTHENTICATED peer
// ---------------------------------------------------------------------------

// TestRelayIngestQuotaIsMeteredByTheAuthenticatedPeer: the applied-key share is
// charged per PROVEN PEER, so a peer cannot buy more of the table by asserting
// more sender labels — and a peer at its share cannot deny the table to another
// peer.
func TestRelayIngestQuotaIsMeteredByTheAuthenticatedPeer(t *testing.T) {
	to := localAgent(t, "bravo")
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeNew}
	fed := newWiringFederation(t, local, 1, 0) // a share of ONE, so the bound is demonstrable
	ctx := context.Background()

	if _, err := fed.acceptRelayFrom(ctx, wiringPeerBus, relayedTo(t, to, wiringPeerBus)); err != nil {
		t.Fatalf("the first message within the share was refused: %v", err)
	}
	before := local.callCount()
	if _, err := fed.acceptRelayFrom(ctx, wiringPeerBus, relayedTo(t, to, wiringPeerBus)); err == nil {
		t.Fatal("a peer past its applied-key share was admitted; the table it is filling fails CLOSED and evicts nothing")
	}
	if local.callCount() != before {
		t.Fatal("the quota refusal still reached the durable write; a bound enforced after the write is not a bound")
	}

	// THE SHARE IS PER PEER. A different authenticated peer has its own.
	if _, err := fed.acceptRelayFrom(ctx, wiringThirdBus, relayedTo(t, to, wiringThirdBus)); err != nil {
		t.Fatalf("a SECOND peer was refused because the first had spent its share: %v", err)
	}
}

// TestRelayIngestQuotaRefundsADuplicate: invariant 10's legitimate retry must
// not be punished. A duplicate creates no applied-key entry, so it costs the
// peer nothing — otherwise a peer retrying correctly after a lost ack would
// spend its budget on messages it never added.
func TestRelayIngestQuotaRefundsADuplicate(t *testing.T) {
	to := localAgent(t, "bravo")
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeRetry}
	fed := newWiringFederation(t, local, 1, 0)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		acc, err := fed.acceptRelayFrom(ctx, wiringPeerBus, relayedTo(t, to, wiringPeerBus))
		if err != nil {
			t.Fatalf("retry %d was refused: %v", i, err)
		}
		if !acc.Duplicate {
			t.Fatalf("retry %d was not reported as a duplicate", i)
		}
	}
	// The budget is intact, so a genuinely new message still fits.
	local.mu.Lock()
	local.outcome = idem.OutcomeNew
	local.mu.Unlock()
	if _, err := fed.acceptRelayFrom(ctx, wiringPeerBus, relayedTo(t, to, wiringPeerBus)); err != nil {
		t.Fatalf("retries consumed the peer's applied-key share: %v", err)
	}
}

// TestRelayIngestConcurrencyIsCappedPerPeer: the second bound, which is what
// keeps a flood of DUPLICATES — which cost no quota — from consuming this
// process without limit.
func TestRelayIngestConcurrencyIsCappedPerPeer(t *testing.T) {
	to := localAgent(t, "bravo")
	gate := make(chan struct{})
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeNew, release: gate}
	fed := newWiringFederation(t, local, 0, 1) // ONE in flight
	ctx := context.Background()

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := fed.acceptRelayFrom(ctx, wiringPeerBus, relayedTo(t, to, wiringPeerBus))
		done <- err
	}()
	<-started
	// Wait until the in-flight slot is definitely taken.
	deadline := time.Now().Add(2 * time.Second)
	for local.callCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first ingest never reached the local bus")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := fed.acceptRelayFrom(ctx, wiringPeerBus, relayedTo(t, to, wiringPeerBus)); err == nil {
		t.Fatal("a peer exceeded its in-flight cap; nothing bounded the relay path before this")
	}
	// A DIFFERENT peer is unaffected: the cap is per peer, not global. Asserted
	// on the meter rather than through a second ingest, which would block on the
	// same gate and prove nothing about the cap.
	release, err := fed.admission.enter(wiringThirdBus)
	if err != nil {
		t.Fatalf("a second peer was refused because the first was busy: %v", err)
	}
	release()

	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("the in-flight ingest failed: %v", err)
	}
	// The slot is released, so the peer can send again.
	if _, err := fed.acceptRelayFrom(ctx, wiringPeerBus, relayedTo(t, to, wiringPeerBus)); err != nil {
		t.Fatalf("the in-flight slot was never released: %v", err)
	}
}

// TestPeerAdmissionCountsLiveEntriesRatherThanRateLimiting pins the meter's
// arithmetic, including the arms a bound is usually wrong in.
//
// The FIRST version of this meter was a refilling token bucket, and the security
// gate showed that it was really a permanent ~20-messages-an-hour speed limit per
// peer that engaged even over an empty table. What is asserted here is the
// replacement: a count of the entries a peer is currently responsible for, which
// never slows a peer under its share and releases in full when the entries expire.
func TestPeerAdmissionCountsLiveEntriesRatherThanRateLimiting(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	adm, err := newPeerAdmission(2, time.Hour, 4, clock, func() bool { return true })
	if err != nil {
		t.Fatalf("newPeerAdmission: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := adm.reserve(wiringPeerBus); err != nil {
			t.Fatalf("charge %d within the share: %v", i, err)
		}
	}
	if _, err := adm.reserve(wiringPeerBus); err == nil {
		t.Fatal("a peer was charged past its share")
	}
	// NO DRIP. Most of the window elapsing releases NOTHING, because the entries
	// those charges stand for have not expired: this is a share, not a rate.
	now = now.Add(59 * time.Minute)
	if _, err := adm.reserve(wiringPeerBus); err == nil {
		t.Fatal("part of the window released part of the share; this meter is a live-entry count, not a token bucket")
	}
	// THE FULL WINDOW RELEASES EVERYTHING, because every entry has now aged out
	// of the applied-key table.
	now = now.Add(2 * time.Minute)
	for i := 0; i < 2; i++ {
		if _, err := adm.reserve(wiringPeerBus); err != nil {
			t.Fatalf("charge %d after the whole window expired: %v", i, err)
		}
	}
	if _, err := adm.reserve(wiringPeerBus); err == nil {
		t.Fatal("the expired window released MORE than the share")
	}
	// A clock that steps BACKWARDS must not release anything early.
	now = now.Add(-10 * time.Hour)
	if _, err := adm.reserve(wiringPeerBus); err == nil {
		t.Fatal("a backwards clock step released the peer's share early")
	}
}

// TestPeerAdmissionOnlyRefusesUnderPressure: below idem's pressure line a peer
// over its share is denying nobody anything, so it is admitted — the same
// posture internal/idem takes for its own per-agent fair share. Without this the
// meter is a permanent speed limit on every federation, enforced over an empty
// table.
func TestPeerAdmissionOnlyRefusesUnderPressure(t *testing.T) {
	pressure := false
	adm, err := newPeerAdmission(1, time.Hour, 4, nil, func() bool { return pressure })
	if err != nil {
		t.Fatalf("newPeerAdmission: %v", err)
	}
	// Far past the share, with the table uncontended: never refused.
	for i := 0; i < 20; i++ {
		if _, err := adm.reserve(wiringPeerBus); err != nil {
			t.Fatalf("charge %d was refused while the applied-key table was NOT under pressure: %v", i, err)
		}
	}
	// The table fills. The bound engages immediately, with the whole window of
	// history behind it — which is why the count is kept even when it is not
	// enforced.
	pressure = true
	if _, err := adm.reserve(wiringPeerBus); err == nil {
		t.Fatal("a peer past its share was admitted while the applied-key table WAS under pressure")
	}
	// FAIL CLOSED: a meter with no pressure signal enforces the bound.
	blind, err := newPeerAdmission(1, time.Hour, 4, nil, nil)
	if err != nil {
		t.Fatalf("newPeerAdmission: %v", err)
	}
	if _, err := blind.reserve(wiringPeerBus); err != nil {
		t.Fatalf("the first charge was refused: %v", err)
	}
	if _, err := blind.reserve(wiringPeerBus); err == nil {
		t.Fatal("a meter built with no pressure signal did not enforce the bound; the fail-closed default is missing")
	}
}

// TestPeerAdmissionReclaimsIdlePeers: the bucket map is capped at relay.MaxPeers,
// so without reclamation the first 64 peers that ever spoke would permanently
// lock out the 65th legitimate one.
func TestPeerAdmissionReclaimsIdlePeers(t *testing.T) {
	adm, err := newPeerAdmission(4, time.Hour, 4, nil, func() bool { return true })
	if err != nil {
		t.Fatalf("newPeerAdmission: %v", err)
	}
	// Fill the map with peers that hold nothing: they only ever entered and left.
	for i := 0; i < relay.MaxPeers; i++ {
		release, err := adm.enter(fmt.Sprintf("bus-%04d", i))
		if err != nil {
			t.Fatalf("peer %d could not enter: %v", i, err)
		}
		release()
	}
	release, err := adm.enter("bus-late")
	if err != nil {
		t.Fatalf("a fresh peer was locked out by %d idle buckets: %v", relay.MaxPeers, err)
	}
	release()

	// A peer holding a LIVE charge is NOT reclaimed, so eviction can never hide
	// one: fill the map with peers that all hold charges, and the next is refused.
	full, err := newPeerAdmission(4, time.Hour, 4, nil, func() bool { return true })
	if err != nil {
		t.Fatalf("newPeerAdmission: %v", err)
	}
	for i := 0; i < relay.MaxPeers; i++ {
		if _, err := full.reserve(fmt.Sprintf("bus-%04d", i)); err != nil {
			t.Fatalf("peer %d could not be charged: %v", i, err)
		}
	}
	if _, err := full.enter("bus-late"); err == nil {
		t.Fatal("a bucket holding a live charge was evicted to make room; the meter would forget entries a peer is responsible for")
	}
}

// ---------------------------------------------------------------------------
// 5. Roster idempotency: the retry is not punished, the violation is refused
// ---------------------------------------------------------------------------

// TestRosterUpdateRetryIsNotPunished covers invariant 10's first two cases on
// the roster surface. Without the memo, an identical retry meets the registry's
// version-monotonicity check and earns a 409 STALE — punishing exactly the peer
// that is retrying correctly after a lost acknowledgement.
func TestRosterUpdateRetryIsNotPunished(t *testing.T) {
	fed := newWiringFederation(t, &stubIngest{}, 0, 0)
	agent, err := ids.AgentID(wiringPeerBus, "alpha", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	if err := fed.acceptPeerFrom(wiringPeerBus, relay.PeerRoster{BusID: wiringPeerBus}); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	u := relay.RosterUpdate{BusID: wiringPeerBus, Version: 1, Added: []string{agent}}
	fp := relay.RosterUpdateFingerprint(u)
	if err := fed.applyRosterFrom(wiringPeerBus, u, "key-1", fp); err != nil {
		t.Fatalf("first update: %v", err)
	}
	// SAME KEY, SAME PAYLOAD: the original result, replayed. Not a 409.
	if err := fed.applyRosterFrom(wiringPeerBus, u, "key-1", fp); err != nil {
		t.Fatalf("an identical retry was refused (%v); invariant 10 requires the ORIGINAL result, not a stale-conflict for retrying correctly", err)
	}

	// SAME KEY, DIFFERENT PAYLOAD: rejected and logged. Nobody is disconnected —
	// there is no disconnect on this path to assert the absence of, and adding
	// one is what this test exists to make visible.
	other := relay.RosterUpdate{BusID: wiringPeerBus, Version: 2, Added: []string{agent}}
	err = fed.applyRosterFrom(wiringPeerBus, other, "key-1", relay.RosterUpdateFingerprint(other))
	if err == nil {
		t.Fatal("one idempotency key carried two different roster deltas and both were applied")
	}
	if !strings.Contains(err.Error(), "idempotency") {
		t.Fatalf("the violation was reported as %v, which the roster handler cannot map to its 409 arm", err)
	}

	// A NEW key still applies normally.
	if err := fed.applyRosterFrom(wiringPeerBus, other, "key-2", relay.RosterUpdateFingerprint(other)); err != nil {
		t.Fatalf("a fresh key was refused: %v", err)
	}
}

// TestPeerEnrolRetryDoesNotResetTheRosterVersion is invariant 10 on the
// HANDSHAKE surface, and the damage it prevents is worse than a status code.
//
// Registry.UpsertPeer REPLACES the peer's roster and RESETS its version to 0. So
// a peer whose handshake acknowledgement was lost, retrying correctly, would
// silently invalidate every roster update it has pushed since — a correct client
// punished with data loss. relay.ValidatePeerEnrollRequest computes the key and
// the fingerprint precisely so the wiring site can prevent that; before this they
// were carried here and dropped.
func TestPeerEnrolRetryDoesNotResetTheRosterVersion(t *testing.T) {
	fed := newWiringFederation(t, &stubIngest{}, 0, 0)
	agent, err := ids.AgentID(wiringPeerBus, "alpha", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	peer := relay.PeerRoster{BusID: wiringPeerBus, IdempotencyKey: "enrol-1"}
	peer.Fingerprint = idem.ComputeFingerprint([]byte(idem.OpPeerEnrol), []byte(peer.BusID))
	if err := fed.acceptPeerFrom(wiringPeerBus, peer); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	u := relay.RosterUpdate{BusID: wiringPeerBus, Version: 7, Added: []string{agent}}
	if err := fed.applyRosterFrom(wiringPeerBus, u, "key-1", relay.RosterUpdateFingerprint(u)); err != nil {
		t.Fatalf("roster update: %v", err)
	}

	// THE RETRY. Same key, same fingerprint: accepted, and the roster version it
	// would have destroyed is intact.
	if err := fed.acceptPeerFrom(wiringPeerBus, peer); err != nil {
		t.Fatalf("an identical handshake retry was refused: %v", err)
	}
	if _, version, _ := fed.registry.Roster(wiringPeerBus); version != 7 {
		t.Fatalf("a handshake RETRY reset the peer's roster version to %d; every roster update it pushed since the first handshake is now invalid", version)
	}
	if !fed.registry.Knows(agent) {
		t.Fatal("a handshake retry discarded the peer's roster")
	}

	// Same key, DIFFERENT roster: invariant 10's violation. Rejected, not applied.
	other := relay.PeerRoster{BusID: wiringPeerBus, IdempotencyKey: "enrol-1", Agents: []string{agent}}
	other.Fingerprint = idem.ComputeFingerprint([]byte(idem.OpPeerEnrol), []byte(other.BusID), []byte(agent))
	err = fed.acceptPeerFrom(wiringPeerBus, other)
	if err == nil {
		t.Fatal("one handshake idempotency key carried two different rosters and both were accepted")
	}
	if !errors.Is(err, relay.ErrIdempotencyViolation) {
		t.Fatalf("the handshake violation was reported as %v, which the handler cannot map to its 409 arm", err)
	}
}

// TestReHandshakeClearsTheRosterMemo: a completed handshake replaces the roster
// and resets the version, so a memo slot surviving it would answer "already
// applied" for an update that is no longer applied to anything — a 200 over a
// registry that never saw it.
func TestReHandshakeClearsTheRosterMemo(t *testing.T) {
	fed := newWiringFederation(t, &stubIngest{}, 0, 0)
	agent, err := ids.AgentID(wiringPeerBus, "alpha", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	if err := fed.acceptPeerFrom(wiringPeerBus, relay.PeerRoster{BusID: wiringPeerBus, IdempotencyKey: "enrol-1"}); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	u := relay.RosterUpdate{BusID: wiringPeerBus, Version: 3, Added: []string{agent}}
	fp := relay.RosterUpdateFingerprint(u)
	if err := fed.applyRosterFrom(wiringPeerBus, u, "key-1", fp); err != nil {
		t.Fatalf("roster update: %v", err)
	}

	// A GENUINELY NEW handshake (different key) — the peer restarted and
	// re-handshook, so its roster is empty and its version is back to 0.
	if err := fed.acceptPeerFrom(wiringPeerBus, relay.PeerRoster{BusID: wiringPeerBus, IdempotencyKey: "enrol-2"}); err != nil {
		t.Fatalf("re-handshake: %v", err)
	}
	if fed.registry.Knows(agent) {
		t.Fatal("the re-handshake did not replace the peer's roster; the fixture is not exercising the case")
	}

	// The peer replays its last roster update. It MUST be applied, not answered
	// "already done" from a memo describing state that no longer exists.
	if err := fed.applyRosterFrom(wiringPeerBus, u, "key-1", fp); err != nil {
		t.Fatalf("the replayed update after a re-handshake was refused: %v", err)
	}
	if !fed.registry.Knows(agent) {
		t.Fatal("after a re-handshake the replayed roster update was answered from a STALE memo: it reported success while the registry never saw it")
	}
}

// TestRefusalsAreFinalRatherThanRetryable pins how each refusal is ANSWERED,
// which is the half no other test here covers — and it uses each handler's OWN
// predicate as the oracle, not relay.ErrorCode, because two of the three
// handlers classify these errors with a direct errors.Is and never consult
// ErrorCode at all (handshake.go:236, rosterhttp.go:184).
//
// A permanent refusal answered as a retryable 503 is not a cosmetic problem: the
// sending bus retries for its whole ~24h horizon and then drops the message.
func TestRefusalsAreFinalRatherThanRetryable(t *testing.T) {
	fed := newWiringFederation(t, &stubIngest{}, 0, 0)

	// PEER ENROL — handshake.go answers ErrPeerRejected with a final 403.
	enrolErr := fed.acceptPeerFrom(wiringPeerBus, relay.PeerRoster{BusID: wiringThirdBus})
	if !errors.Is(enrolErr, relay.ErrPeerRejected) {
		t.Errorf("a peer-enrol claim mismatch (%v) is not ErrPeerRejected, so the handshake handler answers it 503 and the peer retries a refusal it can never satisfy", enrolErr)
	}

	// PEER ENROL, IDEMPOTENCY VIOLATION — must ALSO be final, and must still be
	// recognisable as invariant 10's violation by value.
	peer := relay.PeerRoster{BusID: wiringPeerBus, IdempotencyKey: "k1"}
	peer.Fingerprint = idem.ComputeFingerprint([]byte("a"))
	if err := fed.acceptPeerFrom(wiringPeerBus, peer); err != nil {
		t.Fatalf("first handshake: %v", err)
	}
	clash := relay.PeerRoster{BusID: wiringPeerBus, IdempotencyKey: "k1"}
	clash.Fingerprint = idem.ComputeFingerprint([]byte("b"))
	vErr := fed.acceptPeerFrom(wiringPeerBus, clash)
	if !errors.Is(vErr, relay.ErrIdempotencyViolation) {
		t.Errorf("the handshake key-reuse refusal (%v) is not ErrIdempotencyViolation", vErr)
	}
	if !errors.Is(vErr, relay.ErrPeerRejected) {
		t.Errorf("the handshake key-reuse refusal (%v) is answered 503 RETRYABLE; a peer that reused a key with different content would be told to try again, which guarantees it keeps doing the refused thing", vErr)
	}

	// ROSTER SYNC — rosterhttp.go answers ErrUnknownPeer with a final 403.
	foreign := relay.RosterUpdate{BusID: wiringThirdBus, Version: 1}
	rosterErr := fed.applyRosterFrom(wiringPeerBus, foreign, "k", relay.RosterUpdateFingerprint(foreign))
	if !errors.Is(rosterErr, relay.ErrUnknownPeer) {
		t.Errorf("a roster claim mismatch (%v) is not ErrUnknownPeer, so it is answered 503 instead of a final 403", rosterErr)
	}

	// RELAY INGEST — THE KNOWN GAP, PINNED SO IT CANNOT BE FORGOTTEN.
	// relayhttp.go classifies only ErrUnknownLocalRecipient (404) and
	// ErrIdempotencyViolation (409) and sends everything else to a 503 default,
	// so a PERMANENT claim mismatch is currently answered as retryable. Closing it
	// needs an arm inside internal/relay, outside this task's boundary. This
	// assertion documents the gap and goes RED the day it is fixed, which is the
	// prompt to update it and the note in acceptRelayFrom.
	to := localAgent(t, "bravo")
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeNew}
	metered := newWiringFederation(t, local, 0, 0)
	_, relayErr := metered.acceptRelayFrom(context.Background(), wiringPeerBus, relayedTo(t, to, wiringThirdBus))
	if relayErr == nil {
		t.Fatal("the last-hop mismatch was not refused")
	}
	if errors.Is(relayErr, relay.ErrUnknownLocalRecipient) || errors.Is(relayErr, relay.ErrIdempotencyViolation) {
		t.Errorf("the relay last-hop refusal now matches a sentinel relayhttp classifies (%v): the KNOWN GAP note in acceptRelayFrom and this assertion both need updating", relayErr)
	}
}

// TestTransitMessageIsAcknowledgedLoudly: with no egress wired, a message
// addressed to another bus is durably accepted and carried nowhere. The peer is
// told 200 and will not retry, so those recipients never receive it — that must
// be LOUD in this bus's own log rather than silent.
func TestTransitMessageIsAcknowledgedLoudly(t *testing.T) {
	var logs bytes.Buffer
	to := localAgent(t, "bravo")
	third, err := ids.AgentID(wiringThirdBus, "charlie", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeNew}
	fed, err := newFederation(federationOptions{
		BusID:         wiringLocalBus,
		Registry:      newWiringRegistry(t),
		Local:         local,
		Peers:         newWiringPeerStore(t),
		LocalAgents:   func() []string { return nil },
		Logger:        logging.New(&logs, logging.LevelDebug),
		UnderPressure: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("newFederation: %v", err)
	}

	// Local-only delivery says nothing: this bus did its whole job.
	if _, err := fed.acceptRelayFrom(context.Background(), wiringPeerBus, relayedTo(t, to, wiringPeerBus)); err != nil {
		t.Fatalf("local delivery: %v", err)
	}
	if strings.Contains(logs.String(), "carried NO FURTHER") {
		t.Fatal("a message delivered entirely to local agents was reported as undeliverable transit")
	}

	// A recipient on a THIRD bus is accepted and goes nowhere.
	m := relayedTo(t, to, wiringPeerBus)
	m.Recipients = []string{to, third}
	if _, err := fed.acceptRelayFrom(context.Background(), wiringPeerBus, m); err != nil {
		t.Fatalf("transit message: %v", err)
	}
	if !strings.Contains(logs.String(), "carried NO FURTHER") {
		t.Fatalf("a message accepted for another bus's agents was carried nowhere SILENTLY; log was:\n%s", logs.String())
	}
}

// TestPeerAdmissionChargeSliceIsBoundedByTheShare: below the pressure line a peer
// is admitted past its share, and the meter must NOT keep growing a record of it.
// The security gate measured the original: share=4 and 100 charges gave len=100,
// under a comment claiming the share bounded it.
func TestPeerAdmissionChargeSliceIsBoundedByTheShare(t *testing.T) {
	adm, err := newPeerAdmission(4, time.Hour, 8, nil, func() bool { return false })
	if err != nil {
		t.Fatalf("newPeerAdmission: %v", err)
	}
	for i := 0; i < 100; i++ {
		if _, err := adm.reserve(wiringPeerBus); err != nil {
			t.Fatalf("charge %d below the pressure line was refused: %v", i, err)
		}
	}
	adm.mu.Lock()
	held := len(adm.buckets[wiringPeerBus].charged)
	adm.mu.Unlock()
	if held > 4 {
		t.Fatalf("the meter is holding %d charge instants for one peer with a share of 4; a peer sending freely over an uncontended table grows this without limit", held)
	}
}

// TestUnreplayedPeerRecordsAreCounted: when the peer store cannot be built, the
// federation records in the log are replayed into nothing. auth.MultiplexApplier
// passes over an unregistered kind in COMPLETE SILENCE, so without this counter
// a bus would restore no peer configuration and say nothing about it — the
// silent discard invariant 6 forbids.
func TestUnreplayedPeerRecordsAreCounted(t *testing.T) {
	u := &unreplayedPeerRecords{}
	// relay.OutboxRecordKind is in this list because main.go REGISTERS this
	// applier for it. Until 2026-08-15 it did not appear in Apply's switch, so an
	// outbox record — a delivery this bus OWED a peer — was passed over counting
	// NOTHING, in the very type written to stop that being silent. The old
	// assertion (want 2, over the two config kinds) could not catch it.
	for _, kind := range []string{
		relay.PeerRecordKind, relay.BusTrustRecordKind,
		relay.OutboxRecordKind, relay.OutboxRecordKind,
		"message", "agent",
	} {
		if err := u.Apply(wal.Committed{Entry: wal.Entry{Kind: kind}}); err != nil {
			t.Fatalf("Apply(%s) returned %v; a non-nil error here would poison the log over records this build merely does not serve", kind, err)
		}
	}
	if got := u.ConfigCount(); got != 2 {
		t.Fatalf("counted %d skipped peer-CONFIGURATION records, want 2 (the message and agent kinds belong to other appliers and must not be counted)", got)
	}
	// The two halves are separate because the REMEDIES are: configuration comes
	// back intact on the next start, an owed cross-bus delivery does not come
	// back at all. Rolling them into one number tells an operator "restart and it
	// returns" about the half where that is false.
	if got := u.OutboxCount(); got != 2 {
		t.Fatalf("counted %d skipped relay delivery-OUTBOX records, want 2.\n\nmain.go registers this applier for relay.OutboxRecordKind. A record it does not count is a delivery this bus owed a peer, discarded in complete silence — which invariant 6 rates as the actual defect, not the discard itself.", got)
	}
	if got := u.Count(); got != 4 {
		t.Fatalf("counted %d skipped federation records in total, want 4", got)
	}
}

// ---------------------------------------------------------------------------
// 6. Whether this build federates at all
// ---------------------------------------------------------------------------

// TestBindablePeerCountRequiresAnInboundCertificateBinding: the gate main.go
// uses. A peer store holding signing-key pins but NO inbound client-certificate
// binding can authenticate nobody, and mounting for it would advertise a surface
// that answers 403 to everyone.
func TestBindablePeerCountRequiresAnInboundCertificateBinding(t *testing.T) {
	if got := bindablePeerCount(nil); got != 0 {
		t.Fatalf("bindablePeerCount(nil) = %d, want 0", got)
	}
	dir := t.TempDir()
	dl := &deferredLog{}
	store, err := relay.NewPeerStore(relay.PeerStoreOptions{BusID: wiringLocalBus, Dir: dir, Durable: dl})
	if err != nil {
		t.Fatalf("relay.NewPeerStore: %v", err)
	}
	walLog, err := wal.Open(wal.LogOptions{Dir: dir, Applier: store})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = walLog.Close() })
	dl.log = walLog

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	// Pinned signing keys but NO inbound client certificate: not bindable.
	if _, err := store.PutTrust(relay.BusTrust{BusID: wiringPeerBus, SigningKeys: []ed25519.PublicKey{pub}}); err != nil {
		t.Fatalf("PutTrust: %v", err)
	}
	if got := bindablePeerCount(store); got != 0 {
		t.Fatalf("bindablePeerCount = %d with a trust record carrying no client certificate binding, want 0", got)
	}

	var fp [32]byte
	fp[0] = 1
	if _, err := store.PutTrust(relay.BusTrust{
		BusID:                        wiringPeerBus,
		SigningKeys:                  []ed25519.PublicKey{pub},
		PeerClientTLSCertFingerprint: fp,
	}); err != nil {
		t.Fatalf("PutTrust with a client certificate binding: %v", err)
	}
	if got := bindablePeerCount(store); got != 1 {
		t.Fatalf("bindablePeerCount = %d after binding a peer's client certificate, want 1", got)
	}
}
