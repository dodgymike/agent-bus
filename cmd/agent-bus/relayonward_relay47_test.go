package main

// RELAY-47: ONWARD RELAY — an intermediate bus carries a peer's message to a
// THIRD bus.
//
// # WHAT THIS FILE HAS TO PROVE, AND WHY THE RING TEST IS NOT DECORATION
//
// Until this task, `cmd/agent-bus/relayegress.go`'s "did this message originate
// here?" check (`m.BusPath[0] == busID`) was doing DOUBLE DUTY: it separated a
// local send from a relay ingest, AND — by declining everything that arrived
// from elsewhere — it made every bus a leaf, so no message this bus received
// could ever be put back on the wire. A leaf cannot loop.
//
// Onward relay removes that accidental protection. From here on, the ONLY things
// standing between a cyclic federation and unbounded circulation are the EXPLICIT
// guards in internal/relay/path.go — the egress split horizon (NextHopAllowed),
// the hop stamp (AppendHop) and its limit, and the ingress backstop
// (CheckIncomingPath) — plus, independently, the applied-key table (invariant
// 10, and the two are COMPLEMENTS, never substitutes).
//
// WHAT THIS FILE DOES AND DOES NOT EXERCISE, precisely, because a test that
// claims more than it covers is the thing it is here to prevent: the split
// horizon and the ingress backstop are exercised and were both confirmed by
// mutation. AppendHop is exercised as the HOP STAMP on every forward, but its
// own loop refusal and its store.MaxBusPath limit are NOT reached here — the two
// earlier guards stop a looping copy before AppendHop is asked, and no path in a
// three-bus ring approaches 64 hops. Those two arms are covered by
// internal/relay's own tests, and this header must not be read as covering them.
//
// So this file does not assert "AppendHop is called somewhere". It builds a
// three-bus RING out of the production wiring (newFederation, the real acceptor,
// the real onward seam), pumps a message into it, and measures:
//
//   - the exact number of relay steps a correct ring performs. It is 4, not 6,
//     and the difference IS the split horizon: delete NextHopAllowed and this
//     test fails on the count, not on a vague timeout.
//   - that a peer which does NOT apply the split horizon is still stopped, by
//     the ingress backstop, with relay.ErrRelayLoop.
//   - that the loop guards settle the ring with duplicate suppression OFF, and
//     that duplicate suppression settles it with the path rewritten at every hop
//     — the COMPLEMENT relationship invariant 10 states, each half shown alone.
//   - that with BOTH of those gone the ring does NOT terminate. That subtest is
//     what makes the others mean anything: it demonstrates that the bounded pump
//     can actually run away, so "it terminated" is evidence rather than a
//     tautology.
//
// # THE GUARDS WERE MUTATED OUT, AND THE TEST WENT RED. THAT IS THE EVIDENCE
//
// Stubbing relay.NextHopAllowed to a no-op turns the first subtest RED on the
// step COUNT (6, not 4) and on the backstop drop count; stubbing
// relay.CheckIncomingPath turns three subtests RED. Both were run. An earlier
// draft of the fixture was GREEN under the first mutation — an ordinary routing
// filter was masking the horizon — which is why the origin now holds a recipient
// too (see ringOrigin). A guard test nobody has watched fail is not evidence.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/store"
)

// ---------------------------------------------------------------------------
// Fixtures: a forwarder that records, and a ring of three federations
// ---------------------------------------------------------------------------

// recordingForwarder is a relay.OnwardForwarder that records what it was offered
// and reports how many copies it "queued".
type recordingForwarder struct {
	mu     sync.Mutex
	seen   []relay.RelayedMessage
	queued int
	err    error
}

func (r *recordingForwarder) Enqueue(m relay.RelayedMessage) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, m)
	return r.queued, r.err
}

func (r *recordingForwarder) calls() []relay.RelayedMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]relay.RelayedMessage(nil), r.seen...)
}

// onwardFederation assembles a federation whose onward seam is fwd. share and
// concurrency stay at the production defaults; the quota is told the table is NOT
// under pressure so nothing here is refused for a reason this file is not about.
func onwardFederation(t *testing.T, busID string, local relay.LocalIngest, fwd relay.OnwardForwarder, logs io.Writer) *federation {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	reg, err := relay.NewRegistry(relay.RegistryOptions{BusID: busID})
	if err != nil {
		t.Fatalf("relay.NewRegistry(%s): %v", busID, err)
	}
	peers, err := relay.NewPeerStore(relay.PeerStoreOptions{BusID: busID, Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("relay.NewPeerStore(%s): %v", busID, err)
	}
	fed, err := newFederation(federationOptions{
		BusID:         busID,
		Registry:      reg,
		Local:         local,
		Onward:        fwd,
		Peers:         peers,
		LocalAgents:   func() []string { return nil },
		Logger:        logging.New(logs, logging.LevelDebug),
		UnderPressure: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("newFederation(%s): %v", busID, err)
	}
	return fed
}

// transitMessage is a relayed message that arrived from `from` and names one
// recipient on THIS bus and one on each of `elsewhere`.
func transitMessage(t *testing.T, from string, elsewhere ...string) relay.RelayedMessage {
	t.Helper()
	m := relayedTo(t, localAgent(t, "bravo"), from)
	for _, bus := range elsewhere {
		id, err := ids.AgentID(bus, "charlie", 1)
		if err != nil {
			t.Fatalf("ids.AgentID(%s): %v", bus, err)
		}
		m.Recipients = append(m.Recipients, id)
	}
	return m
}

// ---------------------------------------------------------------------------
// 1. The seam is wired, and it carries the message AS RECEIVED
// ---------------------------------------------------------------------------

// TestOnwardRelayCarriesATransitMessageToAFurtherBus is RELAY-47's headline
// wiring assertion: a message accepted for an agent on a THIRD bus is handed to
// the forwarder instead of stopping here.
//
// It also pins THE TRAP on this path — the two BusPath conventions. What the
// acceptor hands the forwarder is the path AS RECEIVED, WITHOUT this bus's hop:
// appending it is relay.AppendHop's job inside Forward, and a wiring site that
// appended it here would hand AppendHop a path it is already on, so every
// forward would come back ErrRelayLoop about a message that had never left.
func TestOnwardRelayCarriesATransitMessageToAFurtherBus(t *testing.T) {
	var logs bytes.Buffer
	to := localAgent(t, "bravo")
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeNew}
	fwd := &recordingForwarder{queued: 1}
	fed := onwardFederation(t, wiringLocalBus, local, fwd, &logs)

	m := transitMessage(t, wiringPeerBus, wiringThirdBus)
	if _, err := fed.acceptRelayFrom(context.Background(), wiringPeerBus, m); err != nil {
		t.Fatalf("transit message: %v", err)
	}

	calls := fwd.calls()
	if len(calls) != 1 {
		t.Fatalf("the forwarder was offered %d messages, want exactly 1: relay.AcceptOptions.Onward is the seam that carries an ingested message further", len(calls))
	}
	got := calls[0]
	if len(got.BusPath) != 1 || got.BusPath[0] != wiringPeerBus {
		t.Errorf("the onward envelope carries bus_path=%v, want the path AS RECEIVED (%v). Appending our own hop here would make relay.AppendHop refuse every forward as a loop for a message that never left this process", got.BusPath, []string{wiringPeerBus})
	}
	if got.OriginBus != m.OriginBus || got.OriginMessageID != m.OriginMessageID {
		t.Errorf("the onward envelope claims origin %s/%s, want %s/%s carried verbatim: an intermediate re-attests nothing and never claims a message as its own (invariants 1 and 2)",
			got.OriginBus, got.OriginMessageID, m.OriginBus, m.OriginMessageID)
	}
	if len(got.Recipients) != len(m.Recipients) {
		t.Errorf("the onward envelope carries %d recipients, want %d verbatim: the recipient set is covered by the SENDER's signature, so a trimmed list fails verification at the next hop", len(got.Recipients), len(m.Recipients))
	}
	if strings.Contains(logs.String(), "carried NO FURTHER") {
		t.Errorf("a message that WAS carried onward was reported as carried no further; log was:\n%s", logs.String())
	}
}

// TestOnwardRelayIsNotOfferedADuplicate: the re-forward gate is
// idem.OutcomeNew and only idem.OutcomeNew. A duplicate is answered with the
// original result and forwarded NOWHERE — that is what terminates traffic in a
// cyclic topology, and it lives in relay.Acceptor rather than here.
func TestOnwardRelayIsNotOfferedADuplicate(t *testing.T) {
	to := localAgent(t, "bravo")
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeRetry}
	fwd := &recordingForwarder{queued: 1}
	fed := onwardFederation(t, wiringLocalBus, local, fwd, nil)

	acc, err := fed.acceptRelayFrom(context.Background(), wiringPeerBus, transitMessage(t, wiringPeerBus, wiringThirdBus))
	if err != nil {
		t.Fatalf("duplicate transit message: %v", err)
	}
	if !acc.Duplicate {
		t.Fatal("the acceptance did not report a duplicate")
	}
	if n := len(fwd.calls()); n != 0 {
		t.Fatalf("a DUPLICATE was offered onward %d times: re-forwarding a duplicate multiplies exactly the traffic the applied-key table exists to terminate", n)
	}
}

// TestOnwardRelayIsSilentForALocalOnlyDelivery: a message every one of whose
// recipients is ours is not transit at all. Nothing is offered onward and
// nothing is logged about it.
func TestOnwardRelayIsSilentForALocalOnlyDelivery(t *testing.T) {
	var logs bytes.Buffer
	to := localAgent(t, "bravo")
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeNew}
	fwd := &recordingForwarder{queued: 1}
	fed := onwardFederation(t, wiringLocalBus, local, fwd, &logs)

	if _, err := fed.acceptRelayFrom(context.Background(), wiringPeerBus, relayedTo(t, to, wiringPeerBus)); err != nil {
		t.Fatalf("local delivery: %v", err)
	}
	if n := len(fwd.calls()); n != 0 {
		t.Fatalf("a message addressed only to THIS bus's agents was offered onward %d times", n)
	}
	if strings.Contains(logs.String(), "carried NO FURTHER") {
		t.Fatalf("a message this bus delivered in full was reported as undeliverable transit; log was:\n%s", logs.String())
	}
}

// TestOnwardRelayStillWarnsWhenTheForwarderQueuesNothing: the warning must not
// disappear with the wiring. A message with a foreign recipient that no peer
// took — no route, split horizon, hop limit — is still accepted, still 200, and
// still lost, so it must still be LOUD (invariant 6: a discard is never silent).
func TestOnwardRelayStillWarnsWhenTheForwarderQueuesNothing(t *testing.T) {
	var logs bytes.Buffer
	to := localAgent(t, "bravo")
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeNew}
	fwd := &recordingForwarder{queued: 0}
	fed := onwardFederation(t, wiringLocalBus, local, fwd, &logs)

	if _, err := fed.acceptRelayFrom(context.Background(), wiringPeerBus, transitMessage(t, wiringPeerBus, wiringThirdBus)); err != nil {
		t.Fatalf("transit message: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "carried NO FURTHER") {
		t.Fatalf("a message no peer queue accepted was carried nowhere SILENTLY; log was:\n%s", out)
	}
	// The remedy must describe the WIRED bus's actual problem — routing — and must
	// NOT repeat the leaf build's "onward relay needs the egress forwarder, which
	// is not wired yet", which is false here.
	if strings.Contains(out, "which is not wired yet") {
		t.Errorf("the wired build reported the LEAF build's remedy; an operator would go looking for a capability that is already present. Log was:\n%s", out)
	}
	if _, _, noFurther := fed.onward.stats(); noFurther != 1 {
		t.Errorf("carried-no-further count = %d, want 1", noFurther)
	}
}

// TestLeafBuildKeepsItsOwnDiagnosis: with no forwarder the bus is a LEAF, which
// relay documents as a legitimate configuration — and the message it emits must
// stay the one that names the missing capability, because on that build it is
// true. The two cases are reported by different code and must not converge on
// one wording.
func TestLeafBuildKeepsItsOwnDiagnosis(t *testing.T) {
	var logs bytes.Buffer
	to := localAgent(t, "bravo")
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeNew}
	fed := onwardFederation(t, wiringLocalBus, local, nil, &logs)
	if fed.onward != nil {
		t.Fatal("a federation built with no forwarder holds an onward wrapper; nil must stay nil, or relay.Acceptor calls through a non-nil interface holding a nil pointer")
	}

	if _, err := fed.acceptRelayFrom(context.Background(), wiringPeerBus, transitMessage(t, wiringPeerBus, wiringThirdBus)); err != nil {
		t.Fatalf("transit message: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "carried NO FURTHER") || !strings.Contains(out, "no cross-bus forwarder") {
		t.Fatalf("the leaf build did not report the missing capability specifically; log was:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// 2. The bound on peer-triggered onward work
// ---------------------------------------------------------------------------

// TestOnwardFanOutIsBoundedPerMessage: an onward hop is OUTBOUND work a PEER
// asks this bus to do, and each destination peer costs two fsyncs before Enqueue
// returns. A message naming more foreign buses than the bound is accepted (it is
// already durable, and the 200 is already given) and carried NOWHERE, loudly.
func TestOnwardFanOutIsBoundedPerMessage(t *testing.T) {
	to := localAgent(t, "bravo")

	build := func(t *testing.T, foreignBuses int, logs io.Writer) (*federation, *recordingForwarder, relay.RelayedMessage) {
		t.Helper()
		local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeNew}
		fwd := &recordingForwarder{queued: 1}
		fed := onwardFederation(t, wiringLocalBus, local, fwd, logs)
		m := relayedTo(t, to, wiringPeerBus)
		for i := 0; i < foreignBuses; i++ {
			// Distinct DESTINATION buses, which is what the bound counts.
			id, err := ids.AgentID(fmt.Sprintf("faraway%d", i), "charlie", 1)
			if err != nil {
				t.Fatalf("ids.AgentID: %v", err)
			}
			m.Recipients = append(m.Recipients, id)
		}
		return fed, fwd, m
	}

	t.Run("at the bound it is carried", func(t *testing.T) {
		fed, fwd, m := build(t, maxOnwardBusesPerMessage, nil)
		if _, err := fed.acceptRelayFrom(context.Background(), wiringPeerBus, m); err != nil {
			t.Fatalf("transit message: %v", err)
		}
		if n := len(fwd.calls()); n != 1 {
			t.Fatalf("a message naming exactly the permitted %d foreign buses was offered onward %d times, want 1", maxOnwardBusesPerMessage, n)
		}
	})

	t.Run("one over the bound it is refused and said out loud", func(t *testing.T) {
		var logs bytes.Buffer
		fed, fwd, m := build(t, maxOnwardBusesPerMessage+1, &logs)
		if _, err := fed.acceptRelayFrom(context.Background(), wiringPeerBus, m); err != nil {
			t.Fatalf("transit message: %v", err)
		}
		if n := len(fwd.calls()); n != 0 {
			t.Fatalf("a message over the fan-out bound was still offered onward %d times: the bound is not a bound", n)
		}
		out := logs.String()
		if !strings.Contains(out, "carried NO FURTHER") || !strings.Contains(out, "fan_out_limit") {
			t.Fatalf("the fan-out refusal was not reported specifically; log was:\n%s", out)
		}
		if _, refused, _ := fed.onward.stats(); refused != 1 {
			t.Errorf("refused-fan-out count = %d, want 1", refused)
		}
	})

	t.Run("the per-peer product is the stated one", func(t *testing.T) {
		// The bound on ONE PEER's onward work is the product of two numbers, and
		// the arithmetic is asserted rather than described: the in-flight slot is
		// keyed on the AUTHENTICATED peer and is held across the onward enqueue,
		// so a peer can have at most maxConcurrentRelayIngestsPerPeer ingests in
		// flight, each fanning out to at most maxOnwardBusesPerMessage buses.
		if got, want := maxConcurrentRelayIngestsPerPeer*maxOnwardBusesPerMessage, relay.MaxPeers; got != want {
			t.Fatalf("one peer's onward fan-out ceiling is %d, and the comment in relaywiring.go says it is relay.MaxPeers (%d); update BOTH or neither", got, want)
		}
	})
}

// TestOnwardFanOutCountsBusesNotRecipients: many recipients on ONE foreign bus
// are one destination, not many. The bound must not fire on an ordinary message
// addressed to several agents behind the same next hop.
func TestOnwardFanOutCountsBusesNotRecipients(t *testing.T) {
	to := localAgent(t, "bravo")
	local := &stubIngest{enrolled: map[string]bool{to: true}, outcome: idem.OutcomeNew}
	fwd := &recordingForwarder{queued: 1}
	fed := onwardFederation(t, wiringLocalBus, local, fwd, nil)

	m := relayedTo(t, to, wiringPeerBus)
	for i := 1; i <= maxOnwardBusesPerMessage+8; i++ {
		id, err := ids.AgentID(wiringThirdBus, "charlie", uint64(i))
		if err != nil {
			t.Fatalf("ids.AgentID: %v", err)
		}
		m.Recipients = append(m.Recipients, id)
	}
	if _, err := fed.acceptRelayFrom(context.Background(), wiringPeerBus, m); err != nil {
		t.Fatalf("transit message: %v", err)
	}
	if n := len(fwd.calls()); n != 1 {
		t.Fatalf("%d recipients on ONE foreign bus were treated as %d destinations and refused; the bound counts BUSES", len(m.Recipients)-1, len(m.Recipients)-1)
	}
}

// TestOnwardRelayRefusesABroadcastLoudly: a relayed broadcast carries no
// recipient list, so the fan-out count is zero and the seam would otherwise
// return in SILENCE — a message accepted, made durable and dropped from the
// onward path with nothing said about it, which is invariant 6's silent discard.
//
// It is unreachable through the handler today (a relayed broadcast is refused at
// ingest, twice), so this drives the seam directly. The point of the test is the
// day SIGN-3 removes those refusals: the guard must already be here, and it must
// be LOUD.
func TestOnwardRelayRefusesABroadcastLoudly(t *testing.T) {
	var logs bytes.Buffer
	fwd := &recordingForwarder{queued: 1}
	wrapper := newOnwardRelay(wiringLocalBus, fwd, logging.New(&logs, logging.LevelDebug))
	if wrapper == nil {
		t.Fatal("newOnwardRelay returned nil for a non-nil forwarder")
	}

	m := relayedTo(t, localAgent(t, "bravo"), wiringPeerBus)
	m.Broadcast = true
	m.Recipients = nil

	queued, err := wrapper.Enqueue(m)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if queued != 0 {
		t.Fatalf("a relayed broadcast was queued onward %d times; this build has no onward fan-out rule for a message with no recipient list", queued)
	}
	if n := len(fwd.calls()); n != 0 {
		t.Fatalf("a relayed broadcast reached the forwarder %d times", n)
	}
	if !strings.Contains(logs.String(), "relayed BROADCAST") {
		t.Fatalf("a relayed broadcast was dropped from the onward path SILENTLY; log was:\n%s", logs.String())
	}
}

// ---------------------------------------------------------------------------
// 3. The ingress backstop lives in the production validator
// ---------------------------------------------------------------------------

// TestRelayIngressValidatorRefusesALoopBeforeAnythingElse pins the guard the
// ring test below stands in for.
//
// relay.ValidateRelayRequest is what the peer handler calls before AcceptRelay
// ever runs, and CheckIncomingPath is its check 2 — BEFORE the trust store, the
// attestation or the signature is touched. That ordering is why this test can
// pass a NIL CrossBusTrust: if the loop check were moved below verification, or
// deleted, this test would fail rather than quietly stop protecting anything.
func TestRelayIngressValidatorRefusesALoopBeforeAnythingElse(t *testing.T) {
	req := relay.RelayRequest{
		OriginBus: wiringPeerBus,
		MessageID: wiringPeerBus + "-1",
		Sender:    "ignored",
		// THIS BUS IS ALREADY ON THE PATH: the message has been here before.
		BusPath: []string{wiringPeerBus, wiringLocalBus},
	}
	_, err := relay.ValidateRelayRequest(wiringLocalBus, req.MessageID, req, nil)
	if !errors.Is(err, relay.ErrRelayLoop) {
		t.Fatalf("ValidateRelayRequest on a path this bus is already on returned %v, want relay.ErrRelayLoop. The ingress backstop is the half that still works when a peer does not apply the split horizon, and it must run before any per-field or cryptographic work", err)
	}
}

// ---------------------------------------------------------------------------
// 4. The ring: does the traffic terminate once the leaf property is gone?
// ---------------------------------------------------------------------------

const (
	ringBusA = "ringbusa"
	ringBusB = "ringbusb"
	ringBusC = "ringbusc"
)

// ringIngest is a local bus for one ring node: it holds every agent in its own
// namespace and suppresses a second arrival of one origin message id, which is
// what internal/idem does durably in production.
type ringIngest struct {
	busID string

	// noIdem removes duplicate suppression, so a subtest can show what the loop
	// guards do on their OWN. invariant 10 calls the two COMPLEMENTS; this is the
	// switch that lets the test say which one did the work.
	noIdem bool

	mu        sync.Mutex
	seq       uint64
	seen      map[string]string
	delivered []string
}

func newRingIngest(busID string) *ringIngest {
	return &ringIngest{busID: busID, seen: make(map[string]string)}
}

func (r *ringIngest) Enrolled(agentID string) bool {
	bus, _, _, err := ids.ParseAgentID(agentID)
	return err == nil && strings.EqualFold(bus, r.busID)
}

func (r *ringIngest) AcceptRelayed(_ context.Context, m relay.RelayedMessage) (relay.LocalAcceptance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.noIdem {
		if original, dup := r.seen[m.OriginMessageID]; dup {
			return relay.LocalAcceptance{LocalMessageID: original, Outcome: idem.OutcomeRetry}, nil
		}
	}
	r.seq++
	id, err := ids.MessageID(r.busID, r.seq)
	if err != nil {
		return relay.LocalAcceptance{Outcome: idem.OutcomeViolation}, err
	}
	r.seen[m.OriginMessageID] = id
	r.delivered = append(r.delivered, m.OriginMessageID)
	return relay.LocalAcceptance{LocalMessageID: id, Outcome: idem.OutcomeNew}, nil
}

func (r *ringIngest) deliveries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.delivered...)
}

// ringHop is one scheduled outbound relay: who sent it, to whom, and the
// envelope as it goes on the wire.
type ringHop struct {
	from string
	to   *ringNode
	req  relay.RelayRequest
}

// ringPump is the test's message fabric. It is a FIFO rather than depth-first
// recursion for internal/relay/cycle_test.go's reason: breadth-first arrival is
// what produces "one message by two disjoint paths", which is the case loop
// prevention and duplicate suppression have to survive together.
// NOTHING HERE CAN BE SWITCHED OFF ON THE RECEIVING SIDE. Every arrival goes
// through relay.CheckIncomingPath and then through the production
// federation.acceptRelayFrom, in that order, in every subtest. What the subtests
// vary is the behaviour of the SENDING peers, which is the only thing a real bus
// cannot control — so a subtest that runs away does so because of what peers did,
// never because the test disabled this bus's own guard.
type ringPump struct {
	t *testing.T

	mu        sync.Mutex
	hops      []ringHop
	steps     int
	loopDrops int
}

func (p *ringPump) push(h ringHop) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hops = append(p.hops, h)
}

func (p *ringPump) pop() (ringHop, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.hops) == 0 {
		return ringHop{}, false
	}
	h := p.hops[0]
	p.hops = p.hops[1:]
	return h, true
}

// run drains the queue, delivering one relay per step through the RECEIVING
// node's production ingress path (federation.acceptRelayFrom). It reports
// whether the queue emptied inside maxSteps: an unstopped cycle produces work
// for ever, so a bounded pump that empties IS the termination assertion.
func (p *ringPump) run(maxSteps int) (terminated bool) {
	for {
		h, ok := p.pop()
		if !ok {
			return true
		}
		p.mu.Lock()
		p.steps++
		steps := p.steps
		p.mu.Unlock()
		if steps > maxSteps {
			return false
		}

		// THE INGRESS BACKSTOP, exactly as internal/relay applies it: this is
		// check 2 of relay.ValidateRelayRequest, which runs before any other work
		// (pinned by TestRelayIngressValidatorRefusesALoopBeforeAnythingElse). A
		// message that has been here before is DROPPED — answered 200 with
		// dropped_reason=loop on the wire — rather than refused as an error,
		// because in a cyclic topology it is nobody's fault.
		if err := relay.CheckIncomingPath(h.to.busID, h.req.BusPath); err != nil {
			if !errors.Is(err, relay.ErrRelayLoop) {
				p.t.Fatalf("%s refused a path from %s with %v, want relay.ErrRelayLoop", h.to.busID, h.from, err)
			}
			p.mu.Lock()
			p.loopDrops++
			p.mu.Unlock()
			continue
		}

		m := ringDecode(h.req)
		if _, err := h.to.fed.acceptRelayFrom(context.Background(), h.from, m); err != nil {
			p.t.Fatalf("%s accepting from %s: %v", h.to.busID, h.from, err)
		}
	}
}

func (p *ringPump) counters() (steps, loopDrops int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.steps, p.loopDrops
}

// ringDecode turns the wire envelope back into the value the handler builds.
//
// It is check 12 of relay.ValidateRelayRequest and nothing else: the signature
// and the origin attestation are NOT verified here, because this file is about
// routing and loop prevention, and standing up three buses' worth of real
// ed25519 material would prove nothing extra about either. Everything the
// routing decisions depend on — recipients, path, origin, ids — is carried
// exactly as the real decode carries it.
func ringDecode(req relay.RelayRequest) relay.RelayedMessage {
	return relay.RelayedMessage{
		OriginBus:       req.OriginBus,
		OriginMessageID: req.MessageID,
		Sender:          req.Sender,
		Recipients:      append([]string(nil), req.Recipients...),
		BusPath:         append([]string(nil), req.BusPath...),
		Body:            append([]byte(nil), req.Body...),
		ContentSHA256:   req.ContentSHA256,
		IdempotencyKey:  req.MessageID,
	}
}

// ringNode is one bus in the ring: a real federation, its onward forwarder, and
// the peers it relays to.
type ringNode struct {
	busID  string
	fed    *federation
	ingest *ringIngest
	peers  []*ringNode
	fabric *ringPump

	// naive makes this node skip the EGRESS split horizon and offer a copy to
	// EVERY peer, modelling a bus that does not implement the horizon or a peer
	// that does not care. It is how the ingress backstop gets exercised: with the
	// split horizon on, a correct ring never puts a looping copy on the wire at
	// all, so the backstop has nothing to refuse.
	naive bool

	// lying makes this node STRIP THE MIDDLE of the traversed path before
	// forwarding, keeping only the origin hop and its own. It is the limit
	// PROTOCOL.md §8.5 states outright — the path travels OUTSIDE the signature,
	// so a peer can rewrite it — and it is how a subtest switches the path-based
	// guards off entirely in order to show what is left standing.
	//
	// # THE TWO HOPS IT KEEPS ARE NOT DECORATION, AND FINDING OUT WHY IS A RESULT
	//
	// A first version of this stripped the path to the ORIGIN HOP ALONE, which is
	// how internal/relay/cycle_test.go models a liar. The production wiring
	// REFUSED it: cmd/agent-bus/relaywiring.go's checkPeerIsLastHop binds the LAST
	// hop of an incoming path to the authenticated peer, so a peer that erases
	// itself from the path it sends is caught by the one hop this bus can
	// independently verify. A liar's best available move is therefore to keep the
	// origin at the front and ITSELF at the back and delete everything between —
	// which is what this does, and which the wiring cannot detect.
	lying bool
}

// Enqueue implements relay.OnwardForwarder for a ring node. It is the ONE piece
// of the production forwarder this test substitutes, and it substitutes it
// faithfully: relay.Forwarder.targets applies NextHopAllowed per candidate peer,
// and RelayedMessage.Forward stamps this bus's hop through relay.AppendHop —
// both of which are the REAL functions, called here.
func (n *ringNode) Enqueue(m relay.RelayedMessage) (int, error) {
	req, err := m.Forward(n.busID)
	if err != nil {
		// relay.AppendHop refused: we are already on this message's path, or the
		// path is at store.MaxBusPath. Either way nothing goes out — and that is
		// the third explicit guard doing its job, not an error condition.
		return 0, nil
	}
	if n.lying {
		req.BusPath = strippedPath(m.OriginBus, n.busID)
	}
	queued := 0
	for _, p := range n.peers {
		if !n.naive && !relay.NextHopAllowed(m.BusPath, p.busID) {
			// THE EGRESS SPLIT HORIZON: this peer has demonstrably already seen
			// the message, so not one byte leaves this process towards it.
			continue
		}
		if !n.naive && !ringWants(p, m.Recipients) {
			// The routing table, in one line: a correct bus sends a copy to the
			// peer that owns a recipient and to nobody else. relay.Registry.Route
			// is the production answer; a NAIVE node skips it and floods, which is
			// what puts a looping copy on the wire for the backstop to refuse.
			continue
		}
		n.fabric.push(ringHop{from: n.busID, to: p, req: req})
		queued++
	}
	return queued, nil
}

// strippedPath is the shortest path a LYING peer can send and still be admitted
// by the production ingress: the origin first (the path must start at the bus
// the envelope claims as origin) and the sender itself last (checkPeerIsLastHop
// verifies that hop against the TLS-authenticated peer). Everything between —
// the part that carries the loop-prevention history — is gone.
func strippedPath(originBus, selfBus string) []string {
	if strings.EqualFold(originBus, selfBus) {
		return []string{originBus}
	}
	return []string{originBus, selfBus}
}

// ringWants reports whether p holds one of the recipients. The ring is fully
// connected, so "holds a recipient" is the whole routing table.
func ringWants(p *ringNode, recipients []string) bool {
	for _, r := range recipients {
		if bus, _, _, err := ids.ParseAgentID(r); err == nil && strings.EqualFold(bus, p.busID) {
			return true
		}
	}
	return false
}

// TestOnwardRelayRingTerminatesOnTheEXPLICITGuards is the topology test RELAY-47
// requires: with the leaf property gone, does the federation still settle?
func TestOnwardRelayRingTerminatesOnTheEXPLICITGuards(t *testing.T) {
	// The message originates on A and names one agent on B and one on C, so both
	// intermediate buses have a LOCAL delivery AND a foreign recipient — the exact
	// transit shape this task wires.
	t.Run("a correct ring delivers once, terminates, and puts no looping copy on the wire", func(t *testing.T) {
		ring := newRing(t, ringOptions{})
		ring.seed()
		if !ring.pump.run(32) {
			t.Fatal("the ring did not terminate: the message is still circulating, which is what the loop guards exist to prevent")
		}
		steps, loopDrops := ring.pump.counters()
		// FOUR, AND THE NUMBER IS THE ASSERTION. A→B and A→C seed the ring; B
		// forwards to C and C forwards to B. The copies B→A and C→A are NOT sent,
		// because relay.NextHopAllowed sees A already on the path. Delete the split
		// horizon and this becomes 6 — the test fails on a count rather than on a
		// timeout, which is the difference between evidence and a vague feeling.
		if steps != 4 {
			t.Errorf("the ring performed %d relay steps, want 4. The two it must NOT perform are the copies back to the origin, which the EGRESS SPLIT HORIZON (relay.NextHopAllowed) suppresses before a byte leaves the process", steps)
		}
		if loopDrops != 0 {
			t.Errorf("the ingress backstop dropped %d copies in a CORRECT ring; a correct mesh should never put a looping copy on the wire at all, so the backstop should have nothing to do", loopDrops)
		}
		for _, n := range ring.nodes() {
			got := n.ingest.deliveries()
			want := 0
			if n.busID != ringBusA {
				want = 1
			}
			if len(got) != want {
				t.Errorf("%s delivered the message %d times, want %d", n.busID, len(got), want)
			}
		}
	})

	t.Run("a peer with no split horizon is stopped by the ingress backstop", func(t *testing.T) {
		ring := newRing(t, ringOptions{naive: true})
		ring.seed()
		if !ring.pump.run(64) {
			t.Fatal("a ring of buses that do not apply the split horizon did not terminate; the INGRESS backstop is the half that must still work when the egress half does not")
		}
		_, loopDrops := ring.pump.counters()
		if loopDrops == 0 {
			t.Fatal("no copy was dropped by the ingress backstop, so this subtest exercised nothing: a naive ring must put looping copies on the wire for the backstop to refuse")
		}
		if got := len(ring.a.ingest.deliveries()); got != 0 {
			t.Errorf("the ORIGIN bus delivered its own message back to itself %d times", got)
		}
	})

	t.Run("the loop guards alone terminate a ring with no duplicate suppression", func(t *testing.T) {
		// THE SUBTEST THAT ISOLATES THIS TASK'S RISK. Duplicate suppression is
		// OFF, so nothing but the explicit path guards can settle this ring. If
		// relay.NextHopAllowed, relay.AppendHop and relay.CheckIncomingPath were
		// not carrying the load that relayegress.go's leaf property used to carry
		// for free, this is where it shows.
		ring := newRing(t, ringOptions{noIdem: true})
		ring.seed()
		if !ring.pump.run(64) {
			t.Fatal("with duplicate suppression off the ring did not settle, so the explicit loop guards are NOT carrying the loop-prevention load")
		}
	})

	t.Run("duplicate suppression alone terminates a ring whose peers rewrite the path", func(t *testing.T) {
		// The other half of the COMPLEMENT relationship invariant 10 states. Every
		// peer strips the middle of the traversed path, which is metadata outside
		// the signature (PROTOCOL.md §8.5) — so nothing derived from the path can
		// see the cycle any more, and the applied-key table is all that is left.
		// It still terminates.
		ring := newRing(t, ringOptions{naive: true, lying: true})
		ring.seed()
		if !ring.pump.run(64) {
			t.Fatal("with the traversed path rewritten at every hop the ring did not settle on duplicate suppression alone; loop prevention is a COMPLEMENT to idempotency and must never be the only thing standing")
		}
	})

	t.Run("with the path rewritten AND duplicate suppression gone it does NOT terminate", func(t *testing.T) {
		// THE SUBTEST THAT MAKES ALL THE OTHERS MEAN SOMETHING. If this ring could
		// not run away, "it terminated" would be a tautology rather than evidence.
		// Here everything that could stop the traffic is gone at once: the peers
		// rewrite the path they SEND (so the hop stamp and the receiver's backstop
		// see a two-hop path with no cycle in it), they do not apply the split
		// horizon to the path they RECEIVED, and duplicate suppression is off. The
		// bounded pump MUST hit its ceiling.
		//
		// Note which guard is still doing something even here: the ORIGIN never
		// takes its own message back, because a rewritten path still begins with
		// the origin hop and CheckIncomingPath refuses it.
		ring := newRing(t, ringOptions{naive: true, lying: true, noIdem: true})
		ring.seed()
		if ring.pump.run(64) {
			t.Fatal("the ring TERMINATED with every mechanism removed. Either the pump is not actually cyclic — in which case the other subtests prove nothing — or something is still suppressing the traffic and this file no longer knows what is carrying the load")
		}
	})
}

// ---------------------------------------------------------------------------
// Ring construction
// ---------------------------------------------------------------------------

type ringOptions struct {
	naive  bool
	lying  bool
	noIdem bool
}

type ring struct {
	t       *testing.T
	pump    *ringPump
	a, b, c *ringNode
}

func (r *ring) nodes() []*ringNode { return []*ringNode{r.a, r.b, r.c} }

// seed puts the origin's own copies on the queue: A relays to B and to C, with
// the path [A] — which is what relay.AppendHop produces for an ORIGIN, whose
// outbound path is the one legal empty input.
func (r *ring) seed() {
	r.t.Helper()
	m := ringOrigin(r.t)
	if _, err := r.a.Enqueue(m); err != nil {
		r.t.Fatalf("seeding the ring: %v", err)
	}
}

// ringOrigin is the message A's own agent just sent: BusPath EMPTY, because it
// has traversed nothing yet. Forward turns that into exactly [A].
func ringOrigin(t *testing.T) relay.RelayedMessage {
	t.Helper()
	sender, err := ids.AgentID(ringBusA, "alpha", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	// THE ORIGIN HOLDS A RECIPIENT TOO, AND THAT IS WHAT MAKES THE SPLIT HORIZON
	// LOAD-BEARING IN THIS FIXTURE. With recipients only on B and C, an ordinary
	// routing filter ("does this peer hold a recipient?") already suppresses the
	// copies back to A, and relay.NextHopAllowed could be deleted with every
	// assertion below still passing — which was true of the first draft of this
	// test and was caught by mutating the guard out. With a recipient on A as
	// well, B and C both have a routing REASON to send to A, and the horizon is
	// the only thing that stops them.
	var recipients []string
	for _, bus := range []string{ringBusA, ringBusB, ringBusC} {
		id, err := ids.AgentID(bus, "bravo", 1)
		if err != nil {
			t.Fatalf("ids.AgentID: %v", err)
		}
		recipients = append(recipients, id)
	}
	body := []byte("ring")
	return relay.RelayedMessage{
		OriginBus:          ringBusA,
		OriginMessageID:    ringBusA + "-7",
		OriginSeq:          7,
		Sender:             sender,
		Recipients:         recipients,
		BusPath:            nil,
		TimestampUnixMilli: time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC).UnixMilli(),
		Body:               body,
		ContentSHA256:      store.ContentHash(body),
		IdempotencyKey:     ringBusA + "-7",
	}
}

func newRing(t *testing.T, opts ringOptions) *ring {
	t.Helper()
	pump := &ringPump{t: t}
	r := &ring{t: t, pump: pump}
	build := func(busID string) *ringNode {
		ingest := newRingIngest(busID)
		ingest.noIdem = opts.noIdem
		n := &ringNode{busID: busID, ingest: ingest, naive: opts.naive, lying: opts.lying}
		n.fabric = pump
		n.fed = onwardFederation(t, busID, ingest, n, nil)
		return n
	}
	r.a, r.b, r.c = build(ringBusA), build(ringBusB), build(ringBusC)
	// A FULL RING: every bus peers with both others, so a message can come back
	// round to where it started. That is the topology the guards exist for.
	r.a.peers = []*ringNode{r.b, r.c}
	r.b.peers = []*ringNode{r.a, r.c}
	r.c.peers = []*ringNode{r.a, r.b}
	return r
}
