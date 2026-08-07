package relay

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/store"
)

// A three-bus federation built out of REAL components: a real RelayHandler on a
// real TLS server per bus, a real Client per bus, and a real idem.Store per bus
// standing in for the durable applied-key table. Nothing is mocked except the
// local delivery, which is a slice.
//
// The outbound hop is SCHEDULED onto a FIFO rather than performed inline. That
// is not decoration: with depth-first inline forwarding, a node's second copy
// always arrives after that node has already fanned out, so the "same message
// by two disjoint paths" case — the whole reason the fingerprint must exclude
// bus_path — never occurs. A FIFO reproduces the breadth-first arrival order a
// real asynchronous forwarder produces, while staying single-stepped and
// therefore deterministic.

// job is one scheduled outbound relay.
type job struct {
	from *node
	to   *node
	req  RelayRequest
}

// fabric is the test's message pump plus the shared record of what happened.
type fabric struct {
	t *testing.T

	mu   sync.Mutex
	jobs []job

	// answers records every RelayResponse the pump received, in order.
	answers []RelayResponse
}

func (f *fabric) push(j job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, j)
}

func (f *fabric) pop() (job, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.jobs) == 0 {
		return job{}, false
	}
	j := f.jobs[0]
	f.jobs = f.jobs[1:]
	return j, true
}

func (f *fabric) record(resp RelayResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, resp)
}

// run drains the queue, performing one real HTTPS relay per step, and fails if
// the traffic does not terminate. maxSteps is the termination proof: without
// loop prevention a cycle produces work forever, so a bounded pump that empties
// IS the assertion that the mesh settles.
func (f *fabric) run(ctx context.Context, maxSteps int) {
	f.t.Helper()
	for step := 0; ; step++ {
		if step > maxSteps {
			f.t.Fatalf("the federation did not terminate within %d relay steps: the message is still circulating, which is exactly what RELAY-3 exists to prevent", maxSteps)
		}
		j, ok := f.pop()
		if !ok {
			return
		}
		resp, err := j.from.cli.Relay(ctx, j.to.srv.URL, j.req)
		if err != nil {
			f.t.Fatalf("%s relaying to %s: %v", j.from.busID, j.to.busID, err)
		}
		f.record(resp)
	}
}

type node struct {
	fab   *fabric
	busID string
	srv   *httptest.Server
	h     *RelayHandler
	cli   *Client
	keys  *idem.Store

	// peers is who this node relays onward to.
	peers []*node

	// naive makes this node SKIP the egress split horizon, modelling a peer
	// that does not implement it (or lies). It is how the ingress backstop gets
	// exercised: with the split horizon on, a correct mesh never puts a looping
	// copy on the wire at all.
	naive bool

	mu         sync.Mutex
	seq        uint64
	delivered  []string // origin message ids delivered to this bus's own agents
	duplicates int      // arrivals suppressed as idem.OutcomeRetry
	violations int      // arrivals that were idem.OutcomeViolation — must stay 0
}

func newNode(t *testing.T, fab *fabric, busID string) *node {
	t.Helper()
	n := &node{fab: fab, busID: busID, keys: idem.NewStore(idem.StoreOptions{})}
	h, err := NewRelayHandler(RelayConfig{BusID: busID, AcceptRelay: n.accept, Trust: fakeCrossBusTrustForTest})
	if err != nil {
		t.Fatalf("NewRelayHandler(%s): %v", busID, err)
	}
	n.h = h
	n.srv = httptest.NewTLSServer(h)
	t.Cleanup(n.srv.Close)

	cli, err := NewClient(ClientConfig{
		BusID:       busID,
		LocalRoster: func() []string { return nil },
		HTTPClient:  n.srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient(%s): %v", busID, err)
	}
	n.cli = cli
	return n
}

// accept is the wiring site this package deliberately does not own: it consults
// the applied-key table with m.Scope() and m.Fingerprint (invariant 10), mints
// THIS bus's own local id (invariant 1), delivers locally, and only then
// schedules the onward hop.
//
// Scheduling ONLY on a NEW acceptance is what makes the traffic terminate: a
// duplicate is answered and goes no further.
func (n *node) accept(_ context.Context, m RelayedMessage) (RelayAcceptance, error) {
	sc, err := m.Scope()
	if err != nil {
		return RelayAcceptance{}, err
	}

	n.mu.Lock()
	rec, outcome := n.keys.Lookup(sc, m.Fingerprint)
	switch outcome {
	case idem.OutcomeViolation:
		n.violations++
		n.mu.Unlock()
		return RelayAcceptance{}, fmt.Errorf("%w: %s", ErrIdempotencyViolation, m.OriginMessageID)
	case idem.OutcomeRetry:
		n.duplicates++
		n.mu.Unlock()
		var original string
		if err := json.Unmarshal(rec.Result, &original); err != nil {
			return RelayAcceptance{}, fmt.Errorf("stored result is not a message id: %w", err)
		}
		// The ORIGINAL result, replayed verbatim. Nothing re-applied, nobody
		// disconnected (invariant 10).
		return RelayAcceptance{LocalMessageID: original, Duplicate: true}, nil
	}

	n.seq++
	localID, err := ids.MessageID(n.busID, n.seq)
	if err != nil {
		n.mu.Unlock()
		return RelayAcceptance{}, err
	}
	result, err := json.Marshal(localID)
	if err != nil {
		n.mu.Unlock()
		return RelayAcceptance{}, err
	}
	if err := n.keys.Remember(idem.Record{
		Agent:       m.Sender,
		Op:          idem.OpRelay,
		Key:         m.IdempotencyKey,
		Fingerprint: m.Fingerprint,
		Result:      result,
		Seq:         n.seq,
		CommittedAt: time.Now().UTC(),
	}); err != nil {
		n.mu.Unlock()
		return RelayAcceptance{}, err
	}
	n.delivered = append(n.delivered, m.OriginMessageID)
	n.mu.Unlock()

	// Onward, OUTSIDE the lock.
	n.schedule(m)
	return RelayAcceptance{LocalMessageID: localID}, nil
}

// schedule queues one outbound copy per peer the split horizon allows.
func (n *node) schedule(m RelayedMessage) {
	req, err := m.Forward(n.busID)
	if err != nil {
		// Already on the path, or the path is at its cap. Nothing to do.
		return
	}
	for _, p := range n.peers {
		if !n.naive && !NextHopAllowed(m.BusPath, p.busID) {
			continue
		}
		n.fab.push(job{from: n, to: p, req: req})
	}
}

func (n *node) deliveredIDs() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.delivered...)
}

func (n *node) counters() (duplicates, violations int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.duplicates, n.violations
}

// originMessage builds the RelayedMessage a bus holds for a message its OWN
// agent just sent. Its BusPath is EMPTY — the message has traversed nothing yet
// — which Forward turns into exactly [originBus]. See AppendHop.
// mods are applied BEFORE the message is signed, so the result is always a
// message the origin agent could genuinely have produced (SIGN-7).
func originMessage(originBus, sender string, seq uint64, body []byte, mods ...func(*RelayedMessage)) RelayedMessage {
	id, err := ids.MessageID(originBus, seq)
	if err != nil {
		panic(err)
	}
	m := RelayedMessage{
		OriginBus:          originBus,
		OriginMessageID:    id,
		OriginSeq:          seq,
		Sender:             sender,
		Recipients:         []string{"bus-elsewhere.target-1"},
		BusPath:            nil,
		TimestampUnixMilli: time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC).UnixMilli(),
		Body:               body,
		ContentSHA256:      store.ContentHash(body),
		IdempotencyKey:     id,
	}
	for _, mod := range mods {
		mod(&m)
	}
	if err := signRelayedMessage(&m); err != nil {
		// A broadcast (and nothing else the tests build) cannot be canonicalized
		// under format v1, so no signature over it can exist — see SIGN-3 and
		// ValidateRelayRequest check 11a. A right-length placeholder keeps such a
		// fixture usable by the FORWARDING tests, which never verify.
		m.Signature = make([]byte, ed25519.SignatureSize)
	}
	return m
}

// TestRelayLoopPreventionCycle is the headline test for RELAY-3.
//
// It builds GENUINE cycles out of real servers and real clients and proves the
// things that matter:
//
//  1. every bus on the ring delivers the message EXACTLY ONCE;
//  2. the origin never delivers its own message back to itself;
//  3. the traffic TERMINATES — the pump empties inside a bounded step count,
//     whereas an unstopped cycle produces work forever;
//  4. nothing is reported as idem.OutcomeViolation, i.e. no correct peer is
//     ever asked to disconnect another correct peer;
//  5. a peer with NO split horizon is still stopped, by the ingress backstop,
//     with a 200 and dropped_reason=loop rather than an error status.
func TestRelayLoopPreventionCycle(t *testing.T) {
	t.Run("a three bus ring delivers once and terminates", func(t *testing.T) {
		fab := &fabric{t: t}
		a := newNode(t, fab, "bus-a")
		b := newNode(t, fab, "bus-b")
		c := newNode(t, fab, "bus-c")
		// A -> B -> C -> A. A genuine cycle: without loop prevention this
		// circulates forever.
		a.peers = []*node{b}
		b.peers = []*node{c}
		c.peers = []*node{a}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		a.schedule(originMessage("bus-a", "bus-a.alpha-1", 1, []byte("round and round")))
		fab.run(ctx, 32)

		if got := b.deliveredIDs(); len(got) != 1 || got[0] != "bus-a-1" {
			t.Errorf("bus-b delivered %v, want exactly [bus-a-1]", got)
		}
		if got := c.deliveredIDs(); len(got) != 1 || got[0] != "bus-a-1" {
			t.Errorf("bus-c delivered %v, want exactly [bus-a-1]", got)
		}
		if got := a.deliveredIDs(); len(got) != 0 {
			t.Errorf("bus-a delivered its OWN message back to itself: %v", got)
		}
		// The EGRESS split horizon stopped the returning copy at bus-c, so the
		// message never went back on the wire towards bus-a at all. That is the
		// optimisation half of the division of labour doing its job.
		if got := a.h.Stats(); got.LoopDrops != 0 || got.Rejected != 0 || got.Accepted != 0 {
			t.Errorf("bus-a handler stats = %+v, want all zero: the split horizon should stop the cycle before a byte leaves bus-c", got)
		}
		for _, n := range []*node{a, b, c} {
			if _, violations := n.counters(); violations != 0 {
				t.Errorf("%s reported %d idempotency violations; a correct peer must never be asked to disconnect another correct peer", n.busID, violations)
			}
		}
	})

	t.Run("a peer with no split horizon is stopped by the ingress backstop", func(t *testing.T) {
		fab := &fabric{t: t}
		a := newNode(t, fab, "bus-a")
		b := newNode(t, fab, "bus-b")
		c := newNode(t, fab, "bus-c")
		a.peers, b.peers, c.peers = []*node{b}, []*node{c}, []*node{a}
		// bus-c does not apply the split horizon, so it puts a copy on the wire
		// towards the origin. Nothing about that is detectable from the outside:
		// the path is untrusted (PROTOCOL.md §8.5), and the ingress check is the
		// only thing left.
		c.naive = true

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		a.schedule(originMessage("bus-a", "bus-a.alpha-1", 1, []byte("round and round")))
		fab.run(ctx, 32)

		if got := a.h.Stats().LoopDrops; got != 1 {
			t.Errorf("bus-a LoopDrops = %d, want 1: the returning copy must be dropped by the ingress backstop", got)
		}
		if got := a.h.Stats().Rejected; got != 0 {
			t.Errorf("bus-a Rejected = %d, want 0: a loop drop is a settled 200, never a refusal", got)
		}
		if got := a.deliveredIDs(); len(got) != 0 {
			t.Errorf("bus-a delivered its own message back to itself: %v", got)
		}

		// And the answer bus-c saw was SETTLED: 200, accepted:false, loop. An
		// error status would have RELAY-4's retry re-deliver it forever.
		fab.mu.Lock()
		defer fab.mu.Unlock()
		var loopAnswers int
		for _, resp := range fab.answers {
			if !resp.Accepted && resp.DroppedReason == DropLoop {
				loopAnswers++
			}
		}
		if loopAnswers != 1 {
			t.Errorf("the pump saw %d loop answers, want 1; every relay answered 200, so a sender learns 'settled, stop' rather than 'try again'", loopAnswers)
		}
	})

	// THE HARDER TOPOLOGY, and the one that proves the fingerprint excludes
	// bus_path. Every node relays to BOTH others, so each node genuinely
	// receives the message TWICE by two DISJOINT paths — and the second arrival
	// must be suppressed as duplicate:true with HTTP 200, NOT as a violation
	// and NOT as a disconnect.
	t.Run("a fully meshed three cycle suppresses the second arrival as a duplicate", func(t *testing.T) {
		fab := &fabric{t: t}
		a := newNode(t, fab, "bus-a")
		b := newNode(t, fab, "bus-b")
		c := newNode(t, fab, "bus-c")
		a.peers = []*node{b, c}
		b.peers = []*node{a, c}
		c.peers = []*node{a, b}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		a.schedule(originMessage("bus-a", "bus-a.alpha-1", 1, []byte("meshed")))
		fab.run(ctx, 64)

		for _, n := range []*node{b, c} {
			if got := n.deliveredIDs(); len(got) != 1 {
				t.Errorf("%s delivered %v, want EXACTLY ONE copy", n.busID, got)
			}
			duplicates, violations := n.counters()
			if duplicates != 1 {
				t.Errorf("%s suppressed %d duplicates, want 1: in a full mesh it receives the message by two disjoint paths", n.busID, duplicates)
			}
			if violations != 0 {
				t.Errorf("%s reported %d violations; the fingerprint must EXCLUDE bus_path, or every legitimate duplicate in a mesh is a violation and invariant 10 has correct peers disconnect each other as the steady state", n.busID, violations)
			}
			if got := n.h.Stats().Duplicates; got != 1 {
				t.Errorf("%s handler Duplicates = %d, want 1", n.busID, got)
			}
			if got := n.h.Stats().Rejected; got != 0 {
				t.Errorf("%s handler Rejected = %d, want 0: a duplicate is never a refusal, and its sender is never disconnected", n.busID, got)
			}
		}
		if got := a.deliveredIDs(); len(got) != 0 {
			t.Errorf("bus-a delivered its own message back to itself: %v", got)
		}
		if _, violations := a.counters(); violations != 0 {
			t.Errorf("bus-a reported %d violations", violations)
		}

		// Every answer was a 200 that either accepted or duplicate-suppressed.
		fab.mu.Lock()
		defer fab.mu.Unlock()
		for i, resp := range fab.answers {
			if !resp.Accepted {
				t.Errorf("answer %d = %+v, want accepted (a mesh duplicate is accepted-and-duplicate, not dropped)", i, resp)
			}
		}
	})

	// The counterexample, PROVED rather than asserted: a fingerprint that
	// COVERED the path would make the second arrival a violation. This is what
	// makes the shouty comment on relayFingerprint checkable.
	t.Run("covering the path would turn every mesh duplicate into a violation", func(t *testing.T) {
		viaB := relayFixture(func(r *RelayRequest) { r.BusPath = []string{peerBus, "bus-b"} })
		viaC := relayFixture(func(r *RelayRequest) { r.BusPath = []string{peerBus, "bus-c"} })

		one, err := ValidateRelayRequest(localBus, viaB.MessageID, viaB, fakeCrossBusTrustForTest)
		if err != nil {
			t.Fatalf("ValidateRelayRequest: %v", err)
		}
		two, err := ValidateRelayRequest(localBus, viaC.MessageID, viaC, fakeCrossBusTrustForTest)
		if err != nil {
			t.Fatalf("ValidateRelayRequest: %v", err)
		}

		keys := idem.NewStore(idem.StoreOptions{})
		sc, err := one.Scope()
		if err != nil {
			t.Fatalf("Scope: %v", err)
		}
		if err := keys.Remember(idem.Record{
			Agent: one.Sender, Op: idem.OpRelay, Key: one.IdempotencyKey,
			Fingerprint: one.Fingerprint, Result: json.RawMessage(`"bus-local-1"`),
			Seq: 1, CommittedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Remember: %v", err)
		}

		if _, outcome := keys.Lookup(sc, two.Fingerprint); outcome != idem.OutcomeRetry {
			t.Fatalf("the second copy resolved to %v, want %v", outcome, idem.OutcomeRetry)
		}
		// And a hypothetical path-covering fingerprint WOULD be a violation,
		// which is the whole reason the real one excludes the path.
		pathCovering := idem.ComputeFingerprint(one.Fingerprint[:], []byte("bus-c"))
		if _, outcome := keys.Lookup(sc, pathCovering); outcome != idem.OutcomeViolation {
			t.Fatalf("a path-covering fingerprint resolved to %v, want %v — this test's premise is wrong if that is not a violation", outcome, idem.OutcomeViolation)
		}
	})
}

// TestRelayCycleTerminatesEvenWhenAPeerLiesAboutThePath is the backstop's
// limit, stated honestly. A peer that strips ITSELF out of the path defeats the
// sending side's split horizon; a peer that strips US out defeats the receiving
// side's check too. PROTOCOL.md §8.5 says exactly that, and it is why loop
// prevention is availability-only: idempotency, not the path, is what stops the
// message being APPLIED twice.
func TestRelayCycleTerminatesEvenWhenAPeerLiesAboutThePath(t *testing.T) {
	fab := &fabric{t: t}
	a := newNode(t, fab, "bus-a")

	// An honest path naming bus-a is refused by the ingress check.
	honest := relayFixture(func(r *RelayRequest) {
		r.OriginBus = peerBus
		r.BusPath = []string{peerBus, "bus-a"}
	})
	if _, err := ValidateRelayRequest("bus-a", honest.MessageID, honest, fakeCrossBusTrustForTest); !errors.Is(err, ErrRelayLoop) {
		t.Fatalf("the ingress backstop did not fire on an honest path: %v", err)
	}

	// THE LIE: bus-a stripped out. The path check cannot catch this.
	lying := relayFixture(func(r *RelayRequest) {
		r.OriginBus = peerBus
		r.BusPath = []string{peerBus}
	})
	ctx := context.Background()
	first, err := ValidateRelayRequest("bus-a", lying.MessageID, lying, fakeCrossBusTrustForTest)
	if err != nil {
		t.Fatalf("ValidateRelayRequest: %v", err)
	}
	if _, err := a.accept(ctx, first); err != nil {
		t.Fatalf("first accept: %v", err)
	}
	second, err := ValidateRelayRequest("bus-a", lying.MessageID, lying, fakeCrossBusTrustForTest)
	if err != nil {
		t.Fatalf("ValidateRelayRequest: %v", err)
	}
	acc, err := a.accept(ctx, second)
	if err != nil {
		t.Fatalf("second accept: %v", err)
	}
	if !acc.Duplicate {
		t.Fatal("a re-sent message was applied a SECOND time; loop prevention is availability-only, so idempotency is the only thing standing between a lying peer and a double delivery (invariant 10)")
	}
	if got := a.deliveredIDs(); len(got) != 1 {
		t.Fatalf("bus-a delivered %v, want exactly one copy", got)
	}
	if _, violations := a.counters(); violations != 0 {
		t.Fatalf("bus-a reported %d violations for a byte-identical replay; a replay is a RETRY, not a violation", violations)
	}
}
