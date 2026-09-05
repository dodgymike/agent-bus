package main

// RELAY-24-BLOCKER-EGRESS — the two acceptance tests the task's stored
// proof_cmd names, and nothing else.
//
//	TestLocalMessageForPeerRecipientReachesForwarder
//	TestForwarderResumeOrderingSurvivesCrash
//
// # WHY BOTH LIVE IN cmd/agent-bus
//
// Because the seam under test is a COMPOSITION, and every one of its halves is
// already green in isolation. internal/hub has an Egress interface nothing
// implements, internal/relay has a Forwarder with (until this task) zero
// production callers, and both packages' suites stayed green throughout. Only a
// test that assembles the same pieces main.go assembles — hub + registry +
// relayEgress + relay.Forwarder — can tell "the parts exist" from "a message
// leaves this bus".
//
// # THE FAKE PEER NAMES NO PEER ROUTE PATH, DELIBERATELY
//
// internal/relay/guards_test.go bans naming a peer route path (the constant OR
// its string value) outside internal/relay, and calls out "a test that stands up
// a FAKE REMOTE PEER BUS" as a case it refuses. The stand-in below therefore
// answers EVERY path with one catch-all handler and never names one, which
// costs nothing: relay.Client builds the URL itself with peerURL, so the
// request lands wherever the client decides to put it.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	// egLocalBus is the bus under test; egPeerBusID is the FOREIGN bus every
	// remote recipient here is qualified with (invariant 2). egOriginBusID is a
	// THIRD bus, used only as the origin of a message this bus INGESTED.
	egLocalBus   = "egressbus"
	egPeerBusID  = "egresspeer"
	egOriginBus  = "egressorigin"
	egSendOp     = "send"
	egPollWindow = 10 * time.Second
	// egQuietWindow is how long a "nothing more arrived" assertion waits. It is
	// short on purpose: it is only ever the SECOND half of an assertion whose
	// first half already waited for the expected traffic, so a duplicate would
	// have been produced by the same code path, at the same time, on the same
	// goroutines.
	egQuietWindow = 750 * time.Millisecond
)

// egTimestampMs is the SENDER-clock reading every fixture message carries.
// store.NewMessage refuses 0 ("unset"), so a fixture needs a plausible positive
// value, and one shared constant keeps two fixtures from disagreeing about when
// the same fixture traffic happened.
const egTimestampMs int64 = 1754130896789 // 2026-08-02T12:34:56.789Z

// egSignature is a well-formed placeholder. The bus enforces the LENGTH and
// never verifies — it does not hold the sender's messaging key — so any
// signing.SignatureSize bytes are as good as a real signature here.
func egSignature() []byte { return bytes.Repeat([]byte{0xAB}, signing.SignatureSize) }

// egOriginAttestation is the ORIGIN bus's attestation for a foreign sender, as
// every relay-ingest fixture must now carry it (RELAY-48): the hub REQUIRES one,
// because it is the only field of a later ONWARD envelope this bus could never
// regenerate, and accepting an obligation we could not discharge is worse than
// refusing the message.
//
// Well-formed but not genuine, which is the right fidelity here: the durability
// layer validates SHAPE and BINDING-TO-SENDER and never verifies — verification
// needs the origin bus's peering-time pinned signing key and happens in
// internal/relay, before the hub is reached.
func egOriginAttestation(t *testing.T, sender string) attest.Attestation {
	t.Helper()
	return attest.Attestation{
		AgentID:            sender,
		MessagingPublicKey: bytes.Repeat([]byte{0xCD}, ed25519.PublicKeySize),
		IssuedAtUnixMilli:  egTimestampMs,
		NotAfterUnixMilli:  egTimestampMs + 86_400_000,
		Signature:          bytes.Repeat([]byte{0xEF}, signing.SignatureSize),
	}
}

// egSHA256 is the content hash the store computes, recomputed here so an
// assertion compares two independently-derived values rather than one value
// with itself.
func egSHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// The fake peer bus
// ---------------------------------------------------------------------------

// egPeerBus is a stand-in for a peer bus's relay ingress: it accepts everything
// and RECORDS the envelope it was handed, which is the only way to prove a
// forward was not silently dropped somewhere between the hub and the wire.
type egPeerBus struct {
	srv *httptest.Server

	mu  sync.Mutex
	got []relay.RelayRequest
}

func newEgPeerBus(t *testing.T) *egPeerBus {
	t.Helper()
	p := &egPeerBus{}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req relay.RelayRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.got = append(p.got, req)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(relay.RelayResponse{Accepted: true, MessageID: egPeerBusID + "-1"})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// received returns a copy of every envelope this peer has been handed.
func (p *egPeerBus) received() []relay.RelayRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]relay.RelayRequest(nil), p.got...)
}

// await blocks until the peer has received n envelopes, and fails with the
// whole picture if it never does.
func (p *egPeerBus) await(t *testing.T, n int, why string) []relay.RelayRequest {
	t.Helper()
	deadline := time.Now().Add(egPollWindow)
	for {
		got := p.received()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the peer bus received %d relayed envelopes after %s, want %d: %s", len(got), egPollWindow, n, why)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// quiet asserts that no MORE than n envelopes arrive, giving a duplicate a
// window in which to show up.
func (p *egPeerBus) quiet(t *testing.T, n int, why string) {
	t.Helper()
	deadline := time.Now().Add(egQuietWindow)
	for time.Now().Before(deadline) {
		if got := p.received(); len(got) > n {
			t.Fatalf("the peer bus received %d relayed envelopes, want at most %d: %s\ngot: %s", len(got), n, why, egRenderRequests(got))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// byMessageID counts what the peer received, keyed on the ORIGIN message id —
// the only key on which "the same message twice" is a meaningful statement.
func (p *egPeerBus) byMessageID() map[string]int {
	out := map[string]int{}
	for _, r := range p.received() {
		out[r.MessageID]++
	}
	return out
}

func egRenderRequests(reqs []relay.RelayRequest) string {
	var b strings.Builder
	for i, r := range reqs {
		fmt.Fprintf(&b, "  [%d] message_id=%s sender=%s bus_path=%v recipients=%v\n", i, r.MessageID, r.Sender, r.BusPath, r.Recipients)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// The egress seam, observed
// ---------------------------------------------------------------------------

// egRosterStub is the one fact relayEgress reads off the enrolment roster.
type egRosterStub map[string]auth.RosterEntry

func (r egRosterStub) Get(agentID string) (auth.RosterEntry, bool) {
	e, ok := r[agentID]
	return e, ok
}

// egClock is a settable clock. The outbox reads it on every mutating call, and
// a retention window is only observable by moving time.
type egClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *egClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *egClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// egRouterStub is the routing table for a fixture that routes to nobody: every
// recipient is either local or unknown. It is used where the adapter is built
// ONLY to rebuild an envelope, so no routing question is ever asked of it.
type egRouterStub struct{}

func (egRouterStub) Route(string) (string, bool)        { return "", false }
func (egRouterStub) BroadcastTargets([]string) []string { return nil }

// egObservation is what was true AT THE MOMENT the egress seam was reached. It
// exists because the ORDER inside hub.publish is the property under test:
// asserting the same facts after Send returns would pass even if the forward
// happened first.
type egObservation struct {
	durableInWAL  bool
	inServingCopy bool
}

// egSeam is a relayEnqueuer that records every envelope, optionally probes the
// world at the instant it is called, and optionally delegates to a REAL
// relay.Forwarder.
//
// The delegation is what makes the "not dropped" assertion meaningful:
// Forwarder.Enqueue answers (0, nil) both for "no peer routes to this" and for
// "the path says this is a loop", so a recording double alone could not tell an
// accepted forward from a silently discarded one.
type egSeam struct {
	mu    sync.Mutex
	envs  []relay.RelayedMessage
	obs   []egObservation
	probe func(relay.RelayedMessage) egObservation
	inner relayEnqueuer
}

func (s *egSeam) Enqueue(m relay.RelayedMessage) (int, error) {
	s.mu.Lock()
	s.envs = append(s.envs, m)
	probe := s.probe
	s.mu.Unlock()
	if probe != nil {
		o := probe(m)
		s.mu.Lock()
		s.obs = append(s.obs, o)
		s.mu.Unlock()
	}
	if s.inner == nil {
		return 0, nil
	}
	return s.inner.Enqueue(m)
}

func (s *egSeam) envelopes() []relay.RelayedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]relay.RelayedMessage(nil), s.envs...)
}

func (s *egSeam) observations() []egObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]egObservation(nil), s.obs...)
}

// ---------------------------------------------------------------------------
// The fixture: the same pieces main.go composes, minus the network and the TLS
// ---------------------------------------------------------------------------

type egFixture struct {
	dir       string
	log       *wal.Log
	hub       *hub.Hub
	registry  *relay.Registry
	forwarder *relay.Forwarder
	egress    *relayEgress
	seam      *egSeam
	peer      *egPeerBus

	// egressLogs is the ADAPTER's own log, captured rather than discarded.
	//
	// It is the only place the "did this message reach envelope()/attest()?"
	// question is OBSERVABLE. Every failure inside envelope() is a WARN and
	// nothing else — no envelope reaches the seam either way — so a test that
	// watched only the seam cannot tell "declined at the gate" from "reached the
	// attestation and fell over on a roster miss". That distinction is the whole
	// of RELAY-24-BLOCKER-EGRESS's CRIT-2: the second is an ACCIDENTAL defence,
	// and a gate that is only ever proven by it is dead code.
	egressLogs *egSyncBuffer

	busPub ed25519.PublicKey
	msgPub ed25519.PublicKey
	sender string
	local  string // a SECOND agent on THIS bus, the local recipient
	remote string // the recipient behind the peer bus
}

// newEgFixture assembles hub + registry + relayEgress + relay.Forwarder exactly
// as the composition root does. wireEgress=false is a NOT-FEDERATED bus: it omits
// BOTH hub.Options.RemoteRouter and hub.Options.Egress, because they are the two
// halves of one seam and hub.Open refuses a router wired without its egress
// carrier (RELAY-16-FU-SEQUENCING) — production wires the pair together on the
// one peer-store branch.
func newEgFixture(t *testing.T, wireEgress bool) *egFixture {
	t.Helper()

	f := &egFixture{dir: t.TempDir(), peer: newEgPeerBus(t), egressLogs: &egSyncBuffer{}}
	f.sender = egAgentID(t, egLocalBus, "alpha")
	f.local = egAgentID(t, egLocalBus, "beta")
	f.remote = egAgentID(t, egPeerBusID, "gamma")

	lg, err := wal.Open(wal.LogOptions{Dir: f.dir})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", f.dir, err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	f.log = lg

	f.registry = egRegistry(t, f.peer)

	// The bus SIGNING key (which mints the origin attestation) and the SENDER's
	// MESSAGING key (which the attestation binds). Two keypairs, never conflated.
	busPub, busPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the bus signing key: %v", err)
	}
	msgPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the sender's messaging key: %v", err)
	}
	f.busPub, f.msgPub = busPub, msgPub

	f.forwarder = egForwarder(t, f.registry, f.peer, nil, nil)
	f.seam = &egSeam{inner: f.forwarder}

	f.egress, err = newRelayEgress(relayEgressOptions{
		BusID:      egLocalBus,
		SigningKey: busPriv,
		Roster: egRosterStub{f.sender: {
			AgentID:            f.sender,
			Name:               "alpha",
			MessagingPublicKey: msgPub,
			Epoch:              time.Unix(0, egTimestampMs*int64(time.Millisecond)),
		}},
		Forwarder: f.seam,
		// THE SAME REGISTRY THE FORWARDER ROUTES ON, exactly as main.go passes
		// it. The adapter reads it only as a conservative pre-check on whether an
		// attestation is worth minting; handing it a second table here would make
		// the fixture unable to catch a disagreement between the two.
		Router: f.registry,
		Logger: logging.New(f.egressLogs, logging.LevelWarn),
	})
	if err != nil {
		t.Fatalf("newRelayEgress: %v", err)
	}

	// INTERFACE-TYPED, exactly as main.go declares them: handing hub.Options a
	// typed nil would produce a non-nil interface holding a nil pointer, and
	// forwardOnward would call Forward on it.
	//
	// THE ROUTER AND THE EGRESS ARE WIRED AS A PAIR (RELAY-16-FU-SEQUENCING). They
	// are the two halves of one seam — the router ADMITS a recipient behind a peer
	// bus, the egress CARRIES the committed message there — and hub.Open now
	// REFUSES a router wired without its egress carrier, because that config admits
	// a remote recipient it can only make durable and never deliver. Production
	// wires both together on the one peer-store branch, so wireEgress=false is a
	// genuinely NOT-FEDERATED bus (neither wired), which is what "behaves as before
	// the seam" actually means.
	var (
		remoteRouter hub.RemoteRouter
		egress       hub.Egress
	)
	if wireEgress {
		remoteRouter = f.registry
		egress = f.egress
	}

	roster := hub.NewStaticRoster()
	roster.Add(hub.Agent{AgentID: f.sender, Name: "alpha", EnrolledAt: time.Now().Add(-time.Hour)})
	roster.Add(hub.Agent{AgentID: f.local, Name: "beta", EnrolledAt: time.Now().Add(-time.Hour)})

	path := lg.Path()
	h, err := hub.Open(hub.Options{
		BusID:        egLocalBus,
		DataDir:      filepath.Dir(path),
		Durable:      lg,
		Replay:       func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex:    lg.Recovered().NextIndex,
		Roster:       roster,
		RemoteRouter: remoteRouter,
		Egress:       egress,
		Logger:       logging.New(io.Discard, logging.LevelError),
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	f.hub = h
	return f
}

// egAgentID builds the fully-qualified "<bus-id>.<name>-1" (invariant 2).
func egAgentID(t *testing.T, busID, name string) string {
	t.Helper()
	id, err := ids.AgentID(busID, name, 1)
	if err != nil {
		t.Fatalf("ids.AgentID(%q, %q, 1): %v", busID, name, err)
	}
	return id
}

// egRegistry is the routing table, seeded the way main.go seeds it from the
// durable peer store: the peer's bus id with an EMPTY roster, plus its address.
// Registry.Route resolves on the BUS HALF, so an empty roster is the correct
// value and not a stub.
func egRegistry(t *testing.T, peer *egPeerBus) *relay.Registry {
	t.Helper()
	reg, err := relay.NewRegistry(relay.RegistryOptions{BusID: egLocalBus})
	if err != nil {
		t.Fatalf("relay.NewRegistry: %v", err)
	}
	if err := reg.UpsertPeer(relay.PeerRoster{BusID: egPeerBusID}); err != nil {
		t.Fatalf("UpsertPeer(%s): %v", egPeerBusID, err)
	}
	if err := reg.SetPeerBaseURL(egPeerBusID, peer.srv.URL); err != nil {
		t.Fatalf("SetPeerBaseURL(%s, %s): %v", egPeerBusID, peer.srv.URL, err)
	}
	return reg
}

// egForwarder builds the real cross-bus forwarder over the fake peer's TLS
// client. outbox/recover are nil for the non-durable arm.
func egForwarder(t *testing.T, reg *relay.Registry, peer *egPeerBus, outbox *relay.Outbox, recover func(string) (relay.RelayedMessage, bool, error)) *relay.Forwarder {
	t.Helper()
	return egForwarderLogging(t, reg, peer, outbox, recover, io.Discard)
}

// egForwarderLogging is egForwarder with the forwarder's WARN log captured, for
// the one property that is only observable there.
func egForwarderLogging(t *testing.T, reg *relay.Registry, peer *egPeerBus, outbox *relay.Outbox, recover func(string) (relay.RelayedMessage, bool, error), logs io.Writer) *relay.Forwarder {
	t.Helper()
	cli, err := relay.NewClient(relay.ClientConfig{
		BusID:       egLocalBus,
		LocalRoster: func() []string { return nil },
		HTTPClient:  peer.srv.Client(),
	})
	if err != nil {
		t.Fatalf("relay.NewClient: %v", err)
	}
	fwd, err := relay.NewForwarder(relay.ForwarderOptions{
		BusID:    egLocalBus,
		Registry: reg,
		Client:   cli,
		// The registry's own METHOD, as main.go passes it: it is called from
		// every per-peer worker and must be safe for concurrent use.
		PeerBaseURL:    reg.PeerBaseURL,
		Timeout:        5 * time.Second,
		Outbox:         outbox,
		RecoverMessage: recover,
		Logger:         logging.New(logs, logging.LevelWarn),
	})
	if err != nil {
		t.Fatalf("relay.NewForwarder: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = fwd.Close(ctx)
	})
	return fwd
}

// send performs the two-step a real client performs: reserve the assignment
// (Mint), then present it back (Send). A bare Send is not a send any client can
// make since SIGN-1.
func (f *egFixture) send(t *testing.T, to string, body []byte, key string) (hub.Result, error) {
	t.Helper()
	m, err := f.hub.Mint(hub.MintRequest{Sender: f.sender, Op: egSendOp, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("hub.Mint for %q: %v", key, err)
	}
	return f.hub.Send(hub.SendRequest{
		Sender:         f.sender,
		To:             to,
		Body:           body,
		IdempotencyKey: key,
		SignedMint: hub.SignedMint{
			MessageID:          m.MessageID,
			Seq:                m.Seq,
			TimestampUnixMilli: egTimestampMs,
			Signature:          egSignature(),
		},
	})
}

// egWALHasMessage replays the durable log — the FILE, not the in-memory copy —
// and reports whether a committed message record naming id is in it.
//
// It is a read-only pass over the same path hub.Open replays, which is exactly
// what a restart would see. That is the point: "durable" means "a crash right
// now still has it", and only the file can answer that.
func egWALHasMessage(t *testing.T, dir, id string) bool {
	t.Helper()
	found := false
	if _, err := wal.Replay(filepath.Join(dir, wal.WALFileName), func(c wal.Committed) error {
		if c.Entry.Kind == store.RecordKind && bytes.Contains(c.Entry.Body, []byte(`"`+id+`"`)) {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatalf("wal.Replay(%s): %v", dir, err)
	}
	return found
}

// ---------------------------------------------------------------------------
// TEST 1 — a locally-published message for a recipient behind a peer bus
//          reaches Forwarder.Enqueue
// ---------------------------------------------------------------------------

// TestLocalMessageForPeerRecipientReachesForwarder is RELAY-24-BLOCKER-EGRESS's
// acceptance proof, and it asserts the WHOLE chain rather than its last link.
// Each link failed independently before this task and each fails differently:
//
//   - ADMISSION. Without hub.Options.RemoteRouter the send is
//     ErrUnknownRecipient and nothing is written at all.
//   - DURABILITY AND LOCAL DELIVERY FIRST. The forward may never precede the
//     fsync (invariant 4) or the serving copy, so both are probed AT THE INSTANT
//     the seam is reached rather than after Send returns.
//   - THE ENVELOPE. Four fields are traps that fail SILENTLY and universally if
//     they are wrong: an empty BusPath (a stored [busID] makes every forward
//     ErrRelayLoop), the origin message id in BOTH OriginMessageID and
//     IdempotencyKey (ValidateRelayRequest requires that equality at the far
//     end), and a present, verifiable origin attestation (VerifyRelayed refuses
//     an envelope carrying none).
//   - THE FORWARD ACTUALLY LEAVES. Forwarder.Enqueue answers (0, nil) for a
//     dropped job as well as for an unrouted one, so the assertion is the
//     envelope arriving at a fake peer, not a return value.
func TestLocalMessageForPeerRecipientReachesForwarder(t *testing.T) {
	t.Run("a local send for an agent behind a peer bus is admitted, durable, and forwarded once", func(t *testing.T) {
		f := newEgFixture(t, true)
		body := []byte("over the wire")

		// The probe runs INSIDE hub.publish, on the send goroutine, at the exact
		// moment the egress seam is reached.
		f.seam.probe = func(m relay.RelayedMessage) egObservation {
			_, served := f.hub.Store().ByID(m.OriginMessageID)
			return egObservation{
				durableInWAL:  egWALHasMessage(t, f.dir, m.OriginMessageID),
				inServingCopy: served,
			}
		}

		res, err := f.send(t, f.remote, body, "egress-accept")
		if err != nil {
			if errors.Is(err, hub.ErrUnknownRecipient) {
				t.Fatalf("Send to %q was refused as an unknown recipient: %v.\n\nThat is the pre-RELAY-16 answer: hub.Options.RemoteRouter is not wired, so this bus admits nobody behind a peer and there is nothing for the egress seam to carry.", f.remote, err)
			}
			t.Fatalf("Send to the peer-held recipient %q = %v, want it accepted", f.remote, err)
		}

		envs := f.seam.envelopes()
		if len(envs) != 1 {
			t.Fatalf("the egress seam was reached %d times, want exactly 1.\n\nZero means hub.publish never called forwardOnward (or Options.Egress was not wired); more than one means a message is offered to the peer more than once per send.", len(envs))
		}
		env := envs[0]

		obs := f.seam.observations()
		if len(obs) != 1 {
			t.Fatalf("recorded %d probe observations, want 1", len(obs))
		}
		if !obs[0].durableInWAL {
			t.Fatalf("message %s was handed to the cross-bus egress seam BEFORE its record was committed to the write-ahead log.\n\nInvariant 4: nothing is acknowledged before it is durable, and a forward is an acknowledgement to a third party. A crash in that window would leave a peer holding a message this bus has no record of.", res.MessageID)
		}
		if !obs[0].inServingCopy {
			t.Fatalf("message %s was handed to the cross-bus egress seam before it was in this bus's serving copy.\n\nforwardOnward is the LAST statement of publish, after the durable write and after h.notify: a peer must never observe a message a local reader cannot yet see.", res.MessageID)
		}

		// --- the envelope, field by field. Each row names what breaks.
		for _, c := range []struct {
			field string
			got   interface{}
			want  interface{}
			why   string
		}{
			{"OriginBus", env.OriginBus, egLocalBus, "we are the origin: the attesting bus and the origin bus are this bus"},
			{"OriginMessageID", env.OriginMessageID, res.MessageID, "the id THIS bus minted (invariant 1); store.Message.OriginMessageID is empty on a locally-originated message precisely because ID already IS the origin id"},
			{"IdempotencyKey", env.IdempotencyKey, res.MessageID, "relay.ValidateRelayRequest REFUSES an envelope whose key differs from the origin message id; that equality is what makes two copies arriving by two disjoint paths land on ONE applied-key scope (invariant 10)"},
			{"OriginSeq", env.OriginSeq, res.Seq, "the sequence the sender signed"},
			{"Sender", env.Sender, f.sender, "the authenticated local sender, fully qualified (invariant 2)"},
			{"TimestampUnixMilli", env.TimestampUnixMilli, egTimestampMs, "the SIGNED timestamp, not this bus's acceptance clock; substituting SentAt makes every relayed signature fail to verify at the far end, silently and universally"},
			{"ContentSHA256", env.ContentSHA256, egSHA256(body), "the receiving bus re-derives this and checks it against what we declare"},
			{"Broadcast", env.Broadcast, false, "a directed send is not a broadcast"},
			{"len(BusPath)", len(env.BusPath), 0, "an EMPTY path means THIS BUS IS THE ORIGIN. store.Message.BusPath already holds [busID]; copying it here would hand AppendHop a path it is already on, so every forward would come back ErrRelayLoop and be dropped on a bus whose logs would say \"loop\" about a message that never left"},
			{"len(Recipients)", len(env.Recipients), 1, "the recipient set is carried verbatim"},
		} {
			if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
				t.Errorf("envelope.%s = %v, want %v\n  why it matters: %s", c.field, c.got, c.want, c.why)
			}
		}
		if len(env.Recipients) == 1 && env.Recipients[0] != f.remote {
			t.Errorf("envelope.Recipients[0] = %q, want %q", env.Recipients[0], f.remote)
		}
		if !bytes.Equal(env.Body, body) {
			t.Errorf("envelope.Body = %q, want %q", env.Body, body)
		}
		if len(env.Signature) != signing.SignatureSize {
			t.Errorf("envelope.Signature is %d bytes, want %d: a relay carries the signed bytes VERBATIM", len(env.Signature), signing.SignatureSize)
		}

		// --- the origin attestation: present, non-zero, and VERIFIABLE under
		// this bus's signing key. relay.VerifyRelayed refuses an envelope
		// carrying a zero attestation, so a message without one cannot be
		// relayed at all.
		att := env.OriginAttestation
		if att.AgentID == "" || len(att.MessagingPublicKey) == 0 || len(att.Signature) == 0 || att.IssuedAtUnixMilli == 0 || att.NotAfterUnixMilli == 0 {
			t.Fatalf("the envelope carries a ZERO or incomplete origin attestation: agent=%q key=%d bytes sig=%d bytes issued=%d not_after=%d.\n\nrelay.VerifyRelayed refuses an envelope carrying none with ErrMissingAttestation, so this message could never be accepted by a peer.",
				att.AgentID, len(att.MessagingPublicKey), len(att.Signature), att.IssuedAtUnixMilli, att.NotAfterUnixMilli)
		}
		if att.NotAfterUnixMilli <= att.IssuedAtUnixMilli {
			t.Errorf("the attestation expires at or before it was issued (issued=%d not_after=%d)", att.IssuedAtUnixMilli, att.NotAfterUnixMilli)
		}
		gotKey, err := attest.Verify(
			[]ed25519.PublicKey{f.busPub},
			env.OriginAttestation,
			attest.Subject{FQAgentID: f.sender, OriginBus: egLocalBus},
			time.Now(),
		)
		if err != nil {
			t.Fatalf("the origin attestation does not verify under this bus's signing key: %v.\n\nA peer verifies with exactly this call; an attestation that fails here fails there, for every message, for ever.", err)
		}
		if !bytes.Equal(gotKey, f.msgPub) {
			t.Errorf("the attestation binds the sender to the wrong key: it must carry the MESSAGING public key from the roster")
		}

		// --- the trap, both ways round. An empty path yields exactly our own
		// hop; the stored path yields ErrRelayLoop.
		req, err := env.Forward(egLocalBus)
		if err != nil {
			t.Fatalf("envelope.Forward(%q) = %v, want one appended hop.\n\nErrRelayLoop here is the exact failure an eagerly-copied BusPath produces.", egLocalBus, err)
		}
		if len(req.BusPath) != 1 || req.BusPath[0] != egLocalBus {
			t.Fatalf("Forward(%q) produced bus_path %v, want exactly [%s]", egLocalBus, req.BusPath, egLocalBus)
		}
		looped := env
		looped.BusPath = []string{egLocalBus}
		if _, err := looped.Forward(egLocalBus); !errors.Is(err, relay.ErrRelayLoop) {
			t.Fatalf("Forward on an envelope carrying the STORED bus path [%s] = %v, want relay.ErrRelayLoop.\n\nThis row is why the envelope's BusPath must be empty; if it ever stops being an error, the assertion above stops being load-bearing and this test says so.", egLocalBus, err)
		}

		// --- and the forward is NOT DROPPED: it reached the peer.
		got := f.peer.await(t, 1, "a message published locally for an agent behind a peer bus must reach the peer; Forwarder.Enqueue reports (0, nil) for a job it DROPS as well as for one it never routed, so only the peer's own record distinguishes them")
		f.peer.quiet(t, 1, "one local send must produce exactly one cross-bus copy")
		if got[0].MessageID != res.MessageID {
			t.Errorf("the peer received message_id %q, want the origin id %q", got[0].MessageID, res.MessageID)
		}
		if len(got[0].BusPath) != 1 || got[0].BusPath[0] != egLocalBus {
			t.Errorf("the peer received bus_path %v, want exactly [%s]", got[0].BusPath, egLocalBus)
		}
		if got[0].Sender != f.sender {
			t.Errorf("the peer received sender %q, want %q", got[0].Sender, f.sender)
		}
		if got[0].Size != len(body) || got[0].ContentSHA256 != egSHA256(body) {
			t.Errorf("the peer received size=%d sha=%s, want size=%d sha=%s", got[0].Size, got[0].ContentSHA256, len(body), egSHA256(body))
		}

		stats := f.forwarder.Stats()
		if stats.Dropped.Loop != 0 || stats.Dropped.NoRoute != 0 || stats.Dropped.Full != 0 {
			t.Errorf("forwarder dropped jobs: loop=%d no_route=%d full=%d, want none", stats.Dropped.Loop, stats.Dropped.NoRoute, stats.Dropped.Full)
		}
		if stats.Queued != 1 || stats.Sent != 1 {
			t.Errorf("forwarder stats queued=%d sent=%d, want 1 and 1", stats.Queued, stats.Sent)
		}
	})

	t.Run("a message this bus INGESTED from a peer is never re-forwarded", func(t *testing.T) {
		// Onward multi-hop is deliberately out of scope for this seam, and
		// re-forwarding here would be a security defect rather than mere
		// duplication: it would claim OUR bus as the origin of somebody else's
		// message, attest an agent in a foreign namespace, and hand AppendHop an
		// empty path — erasing the loop-prevention history that made the hop safe.
		f := newEgFixture(t, true)
		originID, err := ids.MessageID(egOriginBus, 7)
		if err != nil {
			t.Fatalf("ids.MessageID: %v", err)
		}
		foreignSender := egAgentID(t, egOriginBus, "delta")

		res, err := f.hub.IngestRelayed(context.Background(), hub.RelayedIngestRequest{
			Sender:             foreignSender,
			Recipients:         []string{f.local},
			Body:               []byte("relayed in"),
			OriginMessageID:    originID,
			OriginAttestation:  egOriginAttestation(t, foreignSender),
			BusPath:            []string{egOriginBus},
			TimestampUnixMilli: egTimestampMs,
			Signature:          egSignature(),
		})
		if err != nil {
			t.Fatalf("hub.IngestRelayed: %v", err)
		}
		if res.MessageID == "" {
			t.Fatalf("IngestRelayed returned no local message id")
		}
		// The seam IS reached — publish serves both paths — and the adapter is
		// what declines. Three halves are asserted here: nothing was enqueued,
		// nothing reached the peer, and the adapter LOGGED NOTHING.
		//
		// # NONE OF THESE THREE IS THE RED-ON-MUTATION LINE — see the correction
		//
		// An earlier version of this comment named the "LOGGED NOTHING" check
		// below as what goes red when the bus-path gate is deleted. IT DOES NOT,
		// and the reason is one line up: this ingest addresses ONLY f.local. With
		// the gate removed, Forward falls through to routesToSomePeer, which finds
		// no peer-routable recipient, returns false and declines SILENTLY — same
		// outcome, no envelope, no log, green test.
		//
		// The mutation IS caught, by the table below: its rows address
		// {f.local, f.remote}, so routesToSomePeer answers true, envelope() is
		// entered and attest() fails on the foreign sender with the ~600-byte
		// WARN. Verified by deleting the gate: the ONLY failure was
		// `relayEgress.Forward LOGGED while declining "a foreign bus path and no
		// origin id — WHAT PRODUCTION ACTUALLY BUILDS"`, and this subtest's own
		// three checks all passed.
		//
		// These three are kept as the end-to-end statement that the hub's ingest
		// path reaches the seam at all; the table is the discriminator.
		if envs := f.seam.envelopes(); len(envs) != 0 {
			t.Fatalf("a message INGESTED from %s was offered to the cross-bus forwarder (%d envelopes).\n\nrelayEgress.Forward must decline every message this bus did not ORIGINATE: rebuilding one here claims this bus as its origin and mints an attestation for an agent in another bus's namespace (invariant 2).", egOriginBus, len(envs))
		}
		f.peer.quiet(t, 0, "an ingested message must not be carried onward by this seam")
		// SILENCE, on a recipient set this bus can satisfy LOCALLY. An ingested
		// message must be declined at the GATE, before envelope() is entered. If
		// it reaches envelope() it fails there anyway — the foreign sender is not
		// on this bus's roster — but that is an ACCIDENTAL defence, and it
		// announces itself with a ~600-byte WARN that is both WRONG for an
		// ingested message ("a locally-published message…", with a remedy about
		// missing messaging public keys) and remote-triggerable, once per relayed
		// message, synchronously, under the hub's global write lock.
		//
		// READ THIS CHECK FOR WHAT IT IS: with only a local recipient, the
		// routing pre-check declines this message silently whether or not the
		// gate exists, so an empty buffer here is NOT evidence the gate fired.
		// The table below carries that burden.
		if got := f.egressLogs.String(); got != "" {
			t.Fatalf(`the egress adapter LOGGED while declining a message ingested from %s:

%s
An ingested message must be refused by relayEgress.Forward's bus-path gate, which logs nothing. A line here means it reached envelope()/attest() and was only stopped by the roster miss — a defence that is real but accidental, and one a trusted peer can drive at volume: ~600 bytes written synchronously to stderr under the hub's write lock, per relayed message, naming a fault ("re-enrol that agent with a messaging public key") that has nothing to do with what happened.`, egOriginBus, got)
		}

		// The same rule stated directly against the adapter, so a future change
		// to the hub's ingest path cannot make this pass vacuously.
		//
		// # THE FIRST ROW IS THE ONE THAT MATTERS, AND IT IS WHAT PRODUCTION BUILDS
		//
		// This block used to set ONLY OriginMessageID, and it therefore proved
		// nothing about the shipped build: NOTHING sets store.Message.OriginMessageID
		// (Message.WithOriginMessageID has no non-test caller; the origin id rides
		// on hub's internal publishRequest for the audit hash alone). Deleting the
		// gate left this subtest GREEN while every relay-ingested message went
		// through envelope() and attest(). The load-bearing row is now the one
		// with a foreign BusPath and an EMPTY OriginMessageID — the exact shape
		// store.NewMessageWithBusPath produces on ingest — so removing the bus-path
		// gate turns this red.
		//
		// One recipient is LOCAL to this bus and one is on a third bus this
		// fixture routes to, so the conservative routing pre-check cannot be the
		// thing declining: the message DOES route to a peer, and it must still be
		// refused for being somebody else's.
		//
		// THAT RECIPIENT SET IS THE WHOLE DISCRIMINATOR, and it is the single
		// difference between this table and the IngestRelayed call above (which
		// names only f.local, and is therefore declined by the routing pre-check
		// with or without the gate). Do not "simplify" these rows to one
		// recipient: the row goes green either way the moment f.remote leaves it,
		// and the gate becomes unpinned again.
		for _, row := range []struct {
			name            string
			busPath         []string
			originMessageID string
			wantLog         string // "" means the adapter must say NOTHING
			why             string
		}{
			{
				name:    "a foreign bus path and no origin id — WHAT PRODUCTION ACTUALLY BUILDS",
				busPath: []string{egOriginBus, egLocalBus},
				why:     "store.NewMessageWithBusPath records the path AS RECEIVED with this bus's hop appended, so hop zero is the ORIGIN bus. That is the only field of an ingested message this build sets differently from a local send, and it is what the gate must read",
			},
			{
				name:            "a foreign bus path AND an origin id",
				busPath:         []string{egOriginBus, egLocalBus},
				originMessageID: originID,
				why:             "belt and braces; if store.Message ever starts carrying the origin id, both must still decline",
			},
			{
				name:    "no bus path at all",
				busPath: nil,
				wantLog: "carries NO bus path",
				why:     "structurally unproducible, and DECLINED rather than assumed local: a path-less message names no origin, and forwarding one would assert a provenance nobody recorded. This one IS logged, because unlike an ingest it means something is wrong",
			},
		} {
			f.egressLogs.reset()
			f.egress.Forward(store.Message{
				ID:              res.MessageID,
				Seq:             res.Seq,
				OriginMessageID: row.originMessageID,
				Sender:          foreignSender,
				Recipients:      []string{f.local, f.remote},
				BusPath:         row.busPath,
				Body:            []byte("relayed in"),
			})
			if envs := f.seam.envelopes(); len(envs) != 0 {
				t.Fatalf("relayEgress.Forward enqueued a message that did NOT originate here (%s); it must return without touching the forwarder.\n  why it matters: %s", row.name, row.why)
			}
			got := f.egressLogs.String()
			switch {
			case row.wantLog == "" && got != "":
				t.Fatalf("relayEgress.Forward LOGGED while declining %q:\n\n%s\nIt must decline at the bus-path gate, silently. A line here means it reached envelope()/attest() and was stopped only by the roster miss — an accidental defence, not the stated control.\n  why it matters: %s", row.name, got, row.why)
			case row.wantLog != "" && !strings.Contains(got, row.wantLog):
				t.Fatalf("relayEgress.Forward declining %q logged %q, want a line containing %q.\n  why it matters: %s", row.name, got, row.wantLog, row.why)
			}
		}
	})

	t.Run("cross-bus egress is NOT wedged once a peer's retained records age out", func(t *testing.T) {
		// # THE WEDGE THIS REPRODUCES
		//
		// Outbox.sweepLocked used to ask `_, ok := ob.durable.(outboxCheckpointer)`
		// and, on true, only MARK a swept record expired instead of del()ing it.
		// Only del() decrements retainedByPeer. *wal.Log HAS a Checkpoint method,
		// so that branch was taken the instant the composition root ran
		// Attach(walLog) — but main.go opens the log with NO wal.Checkpoints, so
		// wal.Log.Checkpoint returns "checkpoint requires a MultiApplier"
		// unconditionally, and Outbox.Checkpoint has no production caller anyway.
		// The deferral never resolved: after MaxRetainedPerPeer lifecycle records
		// to one peer, EVERY further Enqueue for that peer returned
		// ErrOutboxCapacity for the life of the process, with every one of those
		// records provably past its retention window. Any enrolled local agent
		// could disable cross-bus egress to a peer permanently.
		//
		// # WHAT IS PRODUCTION WIRING HERE AND WHAT IS NOT
		//
		// wal.Open is main.go's call verbatim (no Checkpoints) and Attach(walLog)
		// is main.go's step 3 — those two are the wedge. MaxRetainedPerPeer and
		// the retention window are overridden ONLY to keep the test in
		// milliseconds: the defect is in the reclaim path and is independent of
		// the bound's value, so 3 records proves what 256 proves and 512 fsyncs
		// fewer.
		dir := t.TempDir()
		walLog, err := wal.Open(wal.LogOptions{Dir: dir})
		if err != nil {
			t.Fatalf("wal.Open(%s): %v", dir, err)
		}
		t.Cleanup(func() { _ = walLog.Close() })
		if walLog.CheckpointSupported() {
			t.Fatalf("this wal.Log reports it can checkpoint; the fixture is meant to be main.go's own wal.Open call, which passes no Checkpoints")
		}
		if err := walLog.Checkpoint(); err == nil {
			t.Fatalf("Checkpoint() succeeded on a log opened with no MultiApplier; the premise of this test (a Checkpoint method that can never run) no longer holds")
		}

		clock := &egClock{now: time.Now().UTC()}
		const retained = 3
		outbox, err := relay.NewOutbox(relay.OutboxOptions{
			BusID:              egLocalBus,
			Logger:             logging.New(io.Discard, logging.LevelError),
			Now:                clock.Now,
			MaxRetainedPerPeer: retained,
			RetryHorizon:       time.Hour,
			SettledRetention:   time.Hour,
		})
		if err != nil {
			t.Fatalf("relay.NewOutbox: %v", err)
		}
		// STEP 3 OF THE PRODUCTION ORDER, and the line that used to flip the
		// sweep onto the branch that never reclaims.
		if err := outbox.Attach(walLog); err != nil {
			t.Fatalf("Outbox.Attach: %v", err)
		}

		enqueue := func(n int) error {
			id, err := ids.MessageID(egLocalBus, uint64(n))
			if err != nil {
				t.Fatalf("ids.MessageID: %v", err)
			}
			body := []byte(fmt.Sprintf("wedge-%d", n))
			rec, err := outbox.Enqueue(relay.OutboxJob{
				PeerBusID:       egPeerBusID,
				OriginMessageID: id,
				Size:            len(body),
				ContentSHA256:   egSHA256(body),
			})
			if err != nil {
				return err
			}
			if _, err := outbox.Settle(rec.JobID, relay.OutboxDelivered, ""); err != nil {
				t.Fatalf("Settle(%s): %v", rec.JobID, err)
			}
			return nil
		}

		// Fill the peer's fair share with DELIVERED tombstones.
		for i := 1; i <= retained; i++ {
			if err := enqueue(i); err != nil {
				t.Fatalf("enqueue %d of %d for %s failed before the limit was reached: %v", i, retained, egPeerBusID, err)
			}
		}
		// One more, while the tombstones are still INSIDE the retention window.
		// Refusing here is correct and is the control for the assertion below: it
		// proves the limit is genuinely being enforced, so a later success cannot
		// be a limit that was never reached.
		if err := enqueue(retained + 1); !errors.Is(err, relay.ErrOutboxCapacity) {
			t.Fatalf("enqueueing past the per-peer retained limit = %v, want relay.ErrOutboxCapacity; the bound is not being enforced and the reclaim assertion below would prove nothing", err)
		}

		// 48 hours: every tombstone is now hours past a one-hour retention window
		// and has no business holding a slot.
		clock.advance(48 * time.Hour)

		if err := enqueue(retained + 2); err != nil {
			t.Fatalf(`CROSS-BUS EGRESS IS WEDGED: enqueueing for %s after every retained record aged out = %v.

Each of the %d retained records is 48 HOURS past its one-hour retention window, so the sweep must have reclaimed their capacity. It did not, which means sweepLocked took the "a checkpoint will reclaim this later" branch on a *wal.Log that can never checkpoint (main.go passes no wal.Checkpoints, and Outbox.Checkpoint has no production caller). Nothing ever calls del(), retainedByPeer never decrements, and this bus will refuse every further cross-bus message to that peer for the life of the process.

See relay.outboxCheckpointReclaims.`, egPeerBusID, err, retained)
		}
		// And it is the WHOLE share that came back, not one slot: the peer now
		// holds one fresh tombstone, so exactly retained-1 more must fit.
		for i := 0; i < retained-1; i++ {
			if err := enqueue(retained + 3 + i); err != nil {
				t.Fatalf("enqueue %d of the %d further messages the reclaimed share must hold = %v; only one slot came back, not the share", i+1, retained-1, err)
			}
		}
		// The share is full again — of records that are all INSIDE the window
		// this time — so the bound is still enforced. A reclaim that had quietly
		// stopped charging would let this through.
		if err := enqueue(3 * retained); !errors.Is(err, relay.ErrOutboxCapacity) {
			t.Fatalf("enqueueing past the refilled per-peer share = %v, want relay.ErrOutboxCapacity; reclaiming capacity must not stop the bound being enforced", err)
		}
	})

	t.Run("a not-federated bus refuses a remote recipient and forwards nothing, exactly as before the seams", func(t *testing.T) {
		// The cheap negative that pins the seam's contract. nil RemoteRouter AND
		// nil Egress is the correct — and, since RELAY-16-FU-SEQUENCING, the only
		// legal — not-federated configuration: hub.Open refuses a router with no
		// egress carrier, so "behaves exactly as before the seams" is the
		// pre-RELAY-16 bus. A remote recipient is refused with the honest 404 and
		// nothing is written for it; a local send still succeeds, is durable, and
		// reaches the nil egress seam zero times without panicking.
		f := newEgFixture(t, false)

		// The remote recipient gets the pre-RELAY-16 answer: with no router this
		// bus admits nobody behind a peer, so the honest refusal stands and nothing
		// is written.
		if _, err := f.send(t, f.remote, []byte("no egress here"), "egress-nil-remote"); !errors.Is(err, hub.ErrUnknownRecipient) {
			t.Fatalf("Send to remote %q on a not-federated bus = %v, want hub.ErrUnknownRecipient; with no router wired the pre-RELAY-16 answer is the honest 404, not accepted-and-never-delivered", f.remote, err)
		}

		// A LOCAL send is unaffected and exercises forwardOnward's nil-egress guard
		// without a panic.
		res, err := f.send(t, f.local, []byte("local still works"), "egress-nil-local")
		if err != nil {
			t.Fatalf("local Send on a not-federated bus = %v, want it to succeed exactly as before", err)
		}
		if _, ok := f.hub.Store().ByID(res.MessageID); !ok {
			t.Fatalf("local message %s is not in the serving copy", res.MessageID)
		}
		if !egWALHasMessage(t, f.dir, res.MessageID) {
			t.Fatalf("local message %s is not in the durable log", res.MessageID)
		}
		if envs := f.seam.envelopes(); len(envs) != 0 {
			t.Fatalf("the egress seam was reached %d times on a hub built without one", len(envs))
		}
		f.peer.quiet(t, 0, "a not-federated bus must forward nothing")
	})
}

// ---------------------------------------------------------------------------
// TEST 2 — the three-stage startup ordering, under a crash
// ---------------------------------------------------------------------------

// egCrashRun is one recovered incarnation of the durable outbox: the pieces
// main.go builds in stages 1-3, assembled in whichever order the test asks for.
type egCrashRun struct {
	dir       string
	log       *wal.Log
	outbox    *relay.Outbox
	registry  *relay.Registry
	forwarder *relay.Forwarder
	logs      *egSyncBuffer
	closed    bool
}

func (r *egCrashRun) close(t *testing.T) {
	t.Helper()
	if r.closed {
		return
	}
	r.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.forwarder.Close(ctx); err != nil {
		t.Fatalf("closing the forwarder: %v", err)
	}
	if err := r.log.Close(); err != nil {
		t.Fatalf("closing the write-ahead log: %v", err)
	}
}

// egSyncBuffer is a mutex-guarded log sink: the forwarder writes from its own
// goroutines, and a bytes.Buffer read from the test goroutine would be a race.
type egSyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *egSyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *egSyncBuffer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

func (s *egSyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// egFixtureMessage is the message a recovered job names. Only the fields the
// envelope builder reads are set; it never goes through a store, because what
// production's RecoverMessage does is exactly this — read a message back and
// hand it to the SAME envelope builder the live path uses.
func egFixtureMessage(t *testing.T, seq uint64, sender, recipient string, body []byte) store.Message {
	t.Helper()
	id, err := ids.MessageID(egLocalBus, seq)
	if err != nil {
		t.Fatalf("ids.MessageID: %v", err)
	}
	return store.Message{
		Seq:                seq,
		ID:                 id,
		Sender:             sender,
		Recipients:         []string{recipient},
		TimestampUnixMilli: egTimestampMs,
		Signature:          egSignature(),
		ContentSHA256:      egSHA256(body),
		Body:               body,
	}
}

// egEnvelopeBuilder returns a relayEgress used ONLY as production's
// RecoverMessage closure uses it: to rebuild an envelope. Its forwarder is a
// recording double that is never consulted.
func egEnvelopeBuilder(t *testing.T, sender string) *relayEgress {
	t.Helper()
	_, busPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the bus signing key: %v", err)
	}
	msgPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the sender's messaging key: %v", err)
	}
	eg, err := newRelayEgress(relayEgressOptions{
		BusID:      egLocalBus,
		SigningKey: busPriv,
		Roster: egRosterStub{sender: {
			AgentID:            sender,
			MessagingPublicKey: msgPub,
			Epoch:              time.Unix(0, egTimestampMs*int64(time.Millisecond)),
		}},
		Forwarder: &egSeam{},
		Router:    egRouterStub{},
		Logger:    logging.New(io.Discard, logging.LevelError),
	})
	if err != nil {
		t.Fatalf("newRelayEgress: %v", err)
	}
	return eg
}

// egCrashWithPendingJob is THE CRASH INJECTION, and the point in the write path
// it kills at is chosen deliberately: AFTER the outbox record is committed and
// fsynced, and BEFORE any settlement.
//
// That is the window relay.Forwarder.Enqueue opens on every forward — the record
// is written first, on purpose, so that the durable set is always a SUPERSET of
// what is in flight — and it is the state a real kill -9 leaves behind. The
// process "dies" by closing the log with the job still PENDING; nothing settles
// it, and nothing gets a chance to clean up.
func egCrashWithPendingJob(t *testing.T, dir string, msgs ...store.Message) []relay.OutboxRecord {
	t.Helper()
	lg, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	ob, err := relay.NewOutbox(relay.OutboxOptions{BusID: egLocalBus})
	if err != nil {
		t.Fatalf("relay.NewOutbox: %v", err)
	}
	if err := ob.Attach(lg); err != nil {
		t.Fatalf("Outbox.Attach: %v", err)
	}
	var recs []relay.OutboxRecord
	for _, m := range msgs {
		rec, err := ob.Enqueue(relay.OutboxJob{
			PeerBusID:       egPeerBusID,
			OriginMessageID: m.ID,
			Size:            len(m.Body),
			ContentSHA256:   m.ContentSHA256,
		})
		if err != nil {
			t.Fatalf("Outbox.Enqueue(%s): %v", m.ID, err)
		}
		if rec.State != relay.OutboxPending {
			t.Fatalf("the job for %s was written as %v, want pending", m.ID, rec.State)
		}
		recs = append(recs, rec)
	}
	// THE CRASH. No settlement, no forwarder drain, no orderly shutdown.
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log to simulate the crash: %v", err)
	}
	return recs
}

// egRecover performs the three startup stages over a crashed data directory, in
// the order the caller asks for.
//
//	stage 1  replay            — wal.Open with the outbox registered as applier
//	stage 2  registry seed     — the peer's id and address (skipped when seed=false)
//	stage 3  Forwarder.Resume  — the CALLER's, so the ordering is the test's subject
func egRecover(t *testing.T, dir string, peer *egPeerBus, seed bool, msgs ...store.Message) *egCrashRun {
	t.Helper()
	run := &egCrashRun{dir: dir, logs: &egSyncBuffer{}}

	// STAGE 1. The applier must exist BEFORE the log, because replay runs inside
	// wal.Open and hands every committed entry to the applier before Open
	// returns. Attach comes after — the same three-step invite.Store follows.
	ob, err := relay.NewOutbox(relay.OutboxOptions{BusID: egLocalBus})
	if err != nil {
		t.Fatalf("relay.NewOutbox: %v", err)
	}
	lg, err := wal.Open(wal.LogOptions{Dir: dir, Applier: ob})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	if err := ob.Attach(lg); err != nil {
		t.Fatalf("Outbox.Attach: %v", err)
	}
	run.outbox, run.log = ob, lg

	// STAGE 2, or its absence.
	reg, err := relay.NewRegistry(relay.RegistryOptions{BusID: egLocalBus})
	if err != nil {
		t.Fatalf("relay.NewRegistry: %v", err)
	}
	if seed {
		if err := reg.UpsertPeer(relay.PeerRoster{BusID: egPeerBusID}); err != nil {
			t.Fatalf("UpsertPeer: %v", err)
		}
		if err := reg.SetPeerBaseURL(egPeerBusID, peer.srv.URL); err != nil {
			t.Fatalf("SetPeerBaseURL: %v", err)
		}
	}
	run.registry = reg

	builder := egEnvelopeBuilder(t, msgs[0].Sender)
	byID := map[string]store.Message{}
	for _, m := range msgs {
		byID[m.ID] = m
	}
	run.forwarder = egForwarderLogging(t, reg, peer, ob, func(originMessageID string) (relay.RelayedMessage, bool, error) {
		m, ok := byID[originMessageID]
		if !ok {
			return relay.RelayedMessage{}, false, nil
		}
		env, err := builder.envelope(m)
		if err != nil {
			return relay.RelayedMessage{}, false, err
		}
		return env, true, nil
	}, run.logs)
	return run
}

// egMessagingAgent is one agent enrolled the way a FEDERATED send requires:
// with a MESSAGING public key on the roster, and holding the private half so it
// can sign what it sends.
//
// The two keys are separate on purpose. busAgent.priv signs the SESSION token
// (auth.SessionSigningContext); msgPriv signs the MESSAGE
// (signing.Canonicalize). Collapsing them would pass here — the bus checks the
// message signature's SHAPE and never its authenticity — and would quietly
// misrepresent what a client actually holds.
type egMessagingAgent struct {
	*busAgent
	msgPriv ed25519.PrivateKey
}

// egEnrolAttestable enrols name WITH a messaging public key and authenticates it.
//
// IT CANNOT BE enrolNewAgent. That helper sends no messaging_public_key, and
// auth.RosterEntry.MessagingPublicKey is OPTIONAL — so an agent enrolled by it
// cannot be ATTESTED to a peer (relayEgress.attest refuses, by design, rather
// than fabricating a key), relayEgress.Forward declines with a WARN, and no
// outbox record is ever written. A test that needs a delivery to be owed needs
// an attestable sender.
func egEnrolAttestable(t *testing.T, dataDir, addr, name string) *egMessagingAgent {
	t.Helper()
	sessPub, sessPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the session keypair for %s: %v", name, err)
	}
	msgPub, msgPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating the messaging keypair for %s: %v", name, err)
	}
	inv := e2eTakeInvite(t, dataDir)
	raw := mustPostJSON(t, dataDir, addr, "/v1/enroll", "", map[string]string{
		"name":                 name,
		"public_key":           base64.StdEncoding.EncodeToString(sessPub),
		"messaging_public_key": base64.StdEncoding.EncodeToString(msgPub),
		"idempotency_key":      fmt.Sprintf("eg-enrol-%s-%d", name, time.Now().UnixNano()),
		"invite_id":            inv.id,
		"invite_secret":        inv.secret,
	}, http.StatusCreated)
	var out struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AgentID == "" {
		t.Fatalf("decoding the enrol response %s: %v", raw, err)
	}
	a := &egMessagingAgent{busAgent: &busAgent{id: out.AgentID, priv: sessPriv}, msgPriv: msgPriv}
	a.authenticate(t, dataDir, addr)
	return a
}

// egSendToPeerRecipient performs the whole reserve-then-send exchange against
// the real routes and returns the message id the bus minted.
//
// It is the shipped two-step (SIGN-1): POST /v1/mint reserves the id and the
// sequence, the CLIENT signs those bytes, and POST /v1/send spends the
// reservation. The recipient is on a PEER bus, so a 201 here means the hub
// admitted it through RemoteRouter and — because hub.forwardOnward runs inside
// publish, before the result is returned — the durable outbox record for the
// cross-bus copy is already committed and fsynced.
func egSendToPeerRecipient(t *testing.T, dataDir, addr string, from *egMessagingAgent, to string, body []byte, key string) string {
	t.Helper()
	minted := mustPostJSON(t, dataDir, addr, "/v1/mint", from.token, map[string]interface{}{
		"op":              egSendOp,
		"idempotency_key": key,
	}, http.StatusCreated)
	var reservation struct {
		MessageID string `json:"message_id"`
		Seq       uint64 `json:"seq"`
	}
	if err := json.Unmarshal(minted, &reservation); err != nil || reservation.MessageID == "" {
		t.Fatalf("decoding the mint response %s: %v", minted, err)
	}

	// THE SENDER'S CLOCK, and it is covered by the signature. The bus stamps its
	// own SentAt separately and neither substitutes for the other.
	ts := time.Now().UTC().UnixMilli()
	sig, err := signing.Sign(from.msgPriv, signing.Message{
		MessageID:          reservation.MessageID,
		Sequence:           reservation.Seq,
		Sender:             from.id,
		Recipients:         []string{to},
		TimestampUnixMilli: ts,
		Body:               body,
	})
	if err != nil {
		t.Fatalf("signing the message for %s: %v", to, err)
	}

	sent := mustPostJSON(t, dataDir, addr, "/v1/send", from.token, map[string]interface{}{
		"to":              to,
		"body":            base64.StdEncoding.EncodeToString(body),
		"idempotency_key": key,
		"sender":          from.id,
		"message_id":      reservation.MessageID,
		"seq":             reservation.Seq,
		"timestamp_ms":    ts,
		"signature":       base64.StdEncoding.EncodeToString(sig),
	}, http.StatusCreated)
	var out struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(sent, &out); err != nil || out.MessageID == "" {
		t.Fatalf("decoding the send response %s: %v", sent, err)
	}
	if out.MessageID != reservation.MessageID {
		t.Fatalf("the send returned message id %q, want the RESERVED %q", out.MessageID, reservation.MessageID)
	}
	return out.MessageID
}

// egAwaitState polls until a job reaches want, and reports what it actually
// settled as if it never does.
func egAwaitState(t *testing.T, ob *relay.Outbox, jobID string, want relay.OutboxState, why string) {
	t.Helper()
	deadline := time.Now().Add(egPollWindow)
	for {
		rec, ok := ob.Lookup(jobID)
		if ok && rec.State == want {
			return
		}
		if time.Now().After(deadline) {
			if !ok {
				t.Fatalf("job %s is not in the outbox at all after %s, want state %v: %s", jobID, egPollWindow, want, why)
			}
			t.Fatalf("job %s settled as %v after %s, want %v (reason: %q): %s", jobID, rec.State, egPollWindow, want, rec.Reason, why)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestForwarderResumeOrderingSurvivesCrash is the crash-injection proof for the
// three-stage startup ordering: peer-store replay, THEN the Registry/roster
// seed, THEN Forwarder.Resume.
//
// Four subtests, and the first is the one that pins the ORDER IN PRODUCTION.
// The rest pin the BEHAVIOUR the order exists to protect, over the real
// relay.Outbox, the real wal.Log and the real relay.Forwarder — because "the
// code looks right" is not evidence for a durability claim.
//
//   - THE STARTUP ORDER ITSELF, read off a real server process's own log. It is
//     order-sensitive by construction: swap the two stages in cmd/agent-bus and
//     the two lines swap with them.
//   - NO FALSE ABANDONMENT. A job recovered from the durable outbox is
//     re-offered when the registry was seeded first, and — critically — is left
//     durably OWED rather than abandoned when it was not. Abandonment is
//     irreversible; a delay is not.
//   - NO DOUBLE-QUEUE. Resume is once-only, and Outbox.Enqueue's idempotent path
//     hands back the existing record rather than creating a second job for the
//     same (peer, message).
func TestForwarderResumeOrderingSurvivesCrash(t *testing.T) {
	t.Run("a real server seeds the routing table BEFORE it resumes the outbox, and the resume RE-OFFERS what it owed", func(t *testing.T) {
		// THE SHIPPED BINARY'S OWN STARTUP PATH, over a data directory that
		// really does owe a peer a delivery. Everything below this subtest could
		// be green while main.go resumed first, or while main.go registered no
		// applier for relay.OutboxRecordKind at all; only this can tell.
		//
		// # WHY THE OWED JOB IS THE WHOLE POINT, AND NOT DECORATION
		//
		// This subtest used to start a bus with an EMPTY outbox and read the
		// POSITION of two lg.Info lines. That was a PROXY for the ordering, not a
		// check of it: moving the Resume() CALL above the registry seed while
		// leaving the two log statements where they are — which IS the bug the
		// subtest exists to catch — left all of it green. It only went red if
		// somebody moved the log lines too.
		//
		// re_offered=1 makes the same assertion SEMANTIC, and closes a second
		// hole with the same line:
		//
		//   - HOISTING Resume() ABOVE THE SEED now goes RED on the number.
		//     Resume resolves every recovered job through PeerBaseURL, so against
		//     an un-seeded registry it takes the no-route arm for the whole
		//     backlog and re-offers ZERO — regardless of where the log lines sit.
		//   - DELETING `appliers[relay.OutboxRecordKind] = relayOutbox` goes RED
		//     too. A job can only be re-offered if replay put its record back, and
		//     auth.MultiplexApplier is deliberately SILENT about kinds it does not
		//     own — so without that registration the record is passed over without
		//     a word and the outbox comes up empty. Nothing else in this package
		//     exercises main.go's applier map on the federated branch: the crash
		//     subtests below build their own wal.Open(Applier: ob).
		dir := t.TempDir()

		// One invite for the one agent below, minted with the bus STOPPED
		// (invariant 3; invitepool_test.go). This call also performs the priming
		// start that gives the directory its bus id AND its certificate — which
		// is what `peer add` needs, since it refuses a directory with no identity
		// and takes the exclusive dirlock the running bus holds.
		e2ePrepareInvites(t, dir, 1)

		// A configured peer, through the operator's real subcommand. Without one
		// the seed stage would seed nothing and the ordering assertion would be
		// satisfied by an empty stage — true, and worthless.
		//
		// THE ADDRESS IS DELIBERATELY DEAD. Port 9 refuses the connection, which
		// relay.Forwarder classifies as RETRIABLE (forward.go retriable: the
		// default is yes, and dial-refused is its named case), so the job is
		// retried against the 24-hour horizon and is never settled. That is what
		// leaves it PENDING for the crash below to preserve.
		code, stdout, stderr := runPeer(t,
			"add",
			"-data-dir", dir,
			"-bus-id", egPeerBusID,
			"-url", "https://127.0.0.1:9",
			"-tls-fingerprint", strings.Repeat("ab", 32),
		)
		if code != exitPeerOK {
			t.Fatalf("`peer add` exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
		}

		// --- THE START THAT ENDS UP OWING A DELIVERY ---
		//
		// A real enrolment and a real send through the real routes, so the outbox
		// record is written by main.go's own wiring rather than forged beside it.
		owing := startServer(t, dir)
		owingAddr := owing.awaitServerStarted(t)
		sender := egEnrolAttestable(t, dir, owingAddr, "alpha")
		body := []byte("owed to a peer across the crash")
		msgID := egSendToPeerRecipient(t, dir, owingAddr, sender,
			egAgentID(t, egPeerBusID, "gamma"), body, "eg-resume-ordering")

		// THE CRASH, and the point in the write path it kills at is what makes
		// the job PENDING rather than settled. By the time /v1/send answered 201
		// the outbox record is committed and fsynced — hub.forwardOnward runs
		// inside publish, before the result is returned, and
		// relay.Forwarder.Enqueue writes the record before it offers the job —
		// while nothing has settled it, because the peer is unreachable and every
		// failure so far is retriable.
		//
		// SIGKILL, not SIGTERM: a graceful stop drains the forwarder, and a
		// settlement written on the way out would be a different test.
		owing.signal(t, syscall.SIGKILL)
		owing.awaitExit(t, shutdownTimeout)

		// --- THE START UNDER TEST ---
		second := startServer(t, dir)
		second.awaitServerStarted(t)

		const (
			seedLine   = "peer routing table seeded"
			resumeLine = "relay delivery outbox resumed"
		)
		second.awaitLine(t, seedLine, startupTimeout)
		second.awaitLine(t, resumeLine, startupTimeout)

		lines := second.snapshot()
		seedAt, resumeAt := -1, -1
		for i, l := range lines {
			if seedAt < 0 && strings.Contains(l, seedLine) {
				seedAt = i
			}
			if resumeAt < 0 && strings.Contains(l, resumeLine) {
				resumeAt = i
			}
		}
		if seedAt < 0 || resumeAt < 0 {
			t.Fatalf("startup logged seed=%d resume=%d (-1 means absent)\n%s", seedAt, resumeAt, second.stderr())
		}
		if seedAt > resumeAt {
			t.Fatalf(`the bus RESUMED the durable relay outbox before it seeded the peer routing table (resume at line %d, seed at line %d).

Forwarder.Resume resolves every recovered job through PeerBaseURL, so a Resume
against an empty registry sees EVERY peer as unknown and takes the no-route arm
for the whole backlog. That arm is fail-safe by design, but relying on it means
a bus that re-offers nothing on every boot and says so only at Warn.

%s`, resumeAt, seedAt, second.stderr())
		}
		if got := parseLogfmt(lines[seedAt])["peers_seeded"]; got != "1" {
			t.Fatalf(`the seed line reports peers_seeded=%q, want "1".

A peer was configured with `+"`peer add`"+` before this start, so a zero here means
the routing table was not restored from the durable peer configuration -- and the
ordering asserted above would then be an ordering between a real stage and an
empty one.

line: %s`, got, lines[seedAt])
		}

		// THE SEMANTIC HALF OF THE ORDERING ASSERTION, symmetric with
		// peers_seeded above. See the doc comment: this is what makes moving the
		// Resume() CALL alone go red, and it is also the only assertion in this
		// package that pins main.go's OutboxRecordKind applier registration.
		if got := parseLogfmt(lines[resumeAt])["re_offered"]; got != "1" {
			t.Fatalf(`the resume line reports re_offered=%q, want "1" (message %s, owed to %s).

Exactly one delivery was owed when this bus was SIGKILLed: a real send through
POST /v1/send for a recipient on a configured peer, whose outbox record was
committed and fsynced before the 201 came back. A zero here means one of:

  * relay.OutboxRecordKind is NOT registered in main.go's applier map, so replay
    passed the record over IN SILENCE (auth.MultiplexApplier says nothing about
    kinds it does not own) and the outbox came up empty; or
  * Forwarder.Resume ran BEFORE the peer routing table was seeded, so every
    recovered job resolved to "peer unknown" and took the no-route arm. The two
    log lines can still be in the right ORDER when this happens -- only the
    number can tell.

Both leave a delivery this bus durably owed a peer un-offered at every boot.

resume line: %s
seed line:   %s
%s`, got, msgID, egPeerBusID, lines[resumeAt], lines[seedAt], second.stderr())
		}
	})

	t.Run("a job recovered from a crash is re-offered, not abandoned", func(t *testing.T) {
		dir := t.TempDir()
		peer := newEgPeerBus(t)
		sender := egAgentID(t, egLocalBus, "alpha")
		msg := egFixtureMessage(t, 11, sender, egAgentID(t, egPeerBusID, "gamma"), []byte("owed to a peer"))

		recs := egCrashWithPendingJob(t, dir, msg)
		jobID := recs[0].JobID
		if want := relay.DeriveJobID(egPeerBusID, msg.ID); jobID != want {
			t.Fatalf("the durable job id is %q, want %q", jobID, want)
		}

		run := egRecover(t, dir, peer, true, msg)
		defer run.close(t)

		// STAGE 1's evidence: the crash left the record on disk and replay put it
		// back. Without the outbox in the applier map this is where the test
		// stops — the record is passed over in silence and the delivery is
		// forgotten (invariant 6's silent-discard defect).
		pending := run.outbox.Pending()
		if len(pending) != 1 || pending[0].JobID != jobID {
			t.Fatalf("after replay the outbox holds %d pending jobs (%v), want exactly the crashed job %s.\n\nA delivery this bus still owed a peer was not recovered: either relay.OutboxRecordKind is not registered as a WAL applier, or the record was discarded.", len(pending), pending, jobID)
		}

		// STAGE 3, after the seed.
		requeued, err := run.forwarder.Resume()
		if err != nil {
			t.Fatalf("Forwarder.Resume: %v", err)
		}
		if requeued != 1 {
			t.Fatalf("Resume re-offered %d jobs, want 1.\n\nThe registry was seeded before this call, so the recovered job had a route; zero here means the recovered backlog is being resolved against an empty routing table.\nforwarder log:\n%s", requeued, run.logs.String())
		}

		got := peer.await(t, 1, "a job recovered from the durable outbox must actually reach the peer once it is re-offered")
		peer.quiet(t, 1, "one recovered job must produce exactly one delivery")
		if got[0].MessageID != msg.ID {
			t.Errorf("the peer received message_id %q, want %q", got[0].MessageID, msg.ID)
		}
		if len(got[0].BusPath) != 1 || got[0].BusPath[0] != egLocalBus {
			t.Errorf("the peer received bus_path %v, want exactly [%s]; the recovered envelope must be rebuilt with an EMPTY path", got[0].BusPath, egLocalBus)
		}

		egAwaitState(t, run.outbox, jobID, relay.OutboxDelivered,
			"a delivered job must be settled durably, or the next start re-offers it for ever")
		if p := run.outbox.Pending(); len(p) != 0 {
			t.Errorf("the outbox still holds %d pending jobs after delivery, want 0", len(p))
		}
	})

	t.Run("resuming before the seed leaves the job OWED, never abandoned", func(t *testing.T) {
		// The failure mode this ordering exists to prevent, driven end to end:
		// the WRONG order must not destroy the recovered backlog. resumeJob's
		// no-route arm is the ONE arm that declines to settle, precisely because
		// at startup "this peer is unknown" is also what a wiring mistake looks
		// like — and it is true of every peer at once.
		dir := t.TempDir()
		peer := newEgPeerBus(t)
		sender := egAgentID(t, egLocalBus, "alpha")
		msg := egFixtureMessage(t, 12, sender, egAgentID(t, egPeerBusID, "gamma"), []byte("owed, and still owed"))

		recs := egCrashWithPendingJob(t, dir, msg)
		jobID := recs[0].JobID

		// The mis-ordered start: stage 3 with stage 2 skipped.
		bad := egRecover(t, dir, peer, false, msg)
		requeued, err := bad.forwarder.Resume()
		if err != nil {
			t.Fatalf("Forwarder.Resume against an empty registry = %v, want no error: the jobs stay owed, they are not a failure", err)
		}
		if requeued != 0 {
			t.Fatalf("Resume re-offered %d jobs against an EMPTY routing table, want 0", requeued)
		}

		rec, ok := bad.outbox.Lookup(jobID)
		if !ok {
			t.Fatalf("job %s vanished from the outbox", jobID)
		}
		if rec.State != relay.OutboxPending {
			t.Fatalf(`job %s was settled %v by a Resume that ran BEFORE the routing table was seeded (reason: %q).

THIS IS THE FALSE ABANDONMENT the ordering exists to prevent. Abandoning here
would destroy the ENTIRE recovered backlog, durably, at boot, from an ordering
bug -- and no restart brings it back. The no-route arm must leave the job OWED.`, jobID, rec.State, rec.Reason)
		}
		peer.quiet(t, 0, "nothing can be delivered to a peer the routing table does not know")

		// Seeding NOW does not rescue this run: Resume is once-only, so the
		// ordering mistake is not silently self-healing. That is the reason it
		// must be got right at startup rather than trusted to fix itself.
		if err := bad.registry.UpsertPeer(relay.PeerRoster{BusID: egPeerBusID}); err != nil {
			t.Fatalf("UpsertPeer: %v", err)
		}
		if err := bad.registry.SetPeerBaseURL(egPeerBusID, peer.srv.URL); err != nil {
			t.Fatalf("SetPeerBaseURL: %v", err)
		}
		if n, err := bad.forwarder.Resume(); !errors.Is(err, relay.ErrForwarderResumed) || n != 0 {
			t.Fatalf("a second Resume returned (%d, %v), want (0, ErrForwarderResumed): Resume is once-only per process, which is what stops a recovered job being offered twice", n, err)
		}
		bad.close(t)

		// And the job is not LOST, only delayed: the next start, with the stages
		// in the right order, delivers it. Recoverable beats irreversible.
		good := egRecover(t, dir, peer, true, msg)
		defer good.close(t)
		if p := good.outbox.Pending(); len(p) != 1 || p[0].JobID != jobID {
			t.Fatalf("after the mis-ordered run and a restart the outbox holds %v, want the job still pending", p)
		}
		requeued, err = good.forwarder.Resume()
		if err != nil {
			t.Fatalf("Forwarder.Resume on the correctly-ordered restart: %v", err)
		}
		if requeued != 1 {
			t.Fatalf("the correctly-ordered restart re-offered %d jobs, want 1\nforwarder log:\n%s", requeued, good.logs.String())
		}
		peer.await(t, 1, "the delivery survives a mis-ordered start and lands on the next correctly-ordered one")
		egAwaitState(t, good.outbox, jobID, relay.OutboxDelivered, "the recovered job is delivered on the restart that had a route")
	})

	t.Run("resume is once-only and a live enqueue cannot double-queue a recovered job", func(t *testing.T) {
		dir := t.TempDir()
		peer := newEgPeerBus(t)
		sender := egAgentID(t, egLocalBus, "alpha")
		recipient := egAgentID(t, egPeerBusID, "gamma")
		owed := egFixtureMessage(t, 21, sender, recipient, []byte("owed from the last run"))
		live := egFixtureMessage(t, 22, sender, recipient, []byte("sent during startup"))

		recs := egCrashWithPendingJob(t, dir, owed)
		owedJob := recs[0].JobID

		run := egRecover(t, dir, peer, true, owed, live)
		defer run.close(t)

		// THE IDEMPOTENT DURABLE PATH, asserted directly: a second Enqueue for a
		// (peer, message) that is ALREADY OWED hands back the EXISTING record
		// rather than minting a second job. That is the mechanism that stops one
		// message becoming two durable deliveries.
		again, err := run.outbox.Enqueue(relay.OutboxJob{
			PeerBusID:       egPeerBusID,
			OriginMessageID: owed.ID,
			Size:            len(owed.Body),
			ContentSHA256:   owed.ContentSHA256,
		})
		if err != nil {
			t.Fatalf("re-enqueueing an already-pending job = %v, want the existing record", err)
		}
		if again.JobID != owedJob {
			t.Fatalf("re-enqueueing produced job id %q, want the existing %q", again.JobID, owedJob)
		}
		if n := run.outbox.Len(); n != 1 {
			t.Fatalf("the outbox holds %d records after a repeat enqueue, want 1: one (peer, message) is one job", n)
		}

		// STAGE 3, and only then live traffic — the production order, which is
		// what makes "no double-queue" a statement with content: Resume runs
		// BEFORE the forwarder is put in service, so nothing can race the pass.
		resumed, err := run.forwarder.Resume()
		if err != nil {
			t.Fatalf("Forwarder.Resume: %v", err)
		}
		if resumed != 1 {
			t.Fatalf("Resume re-offered %d jobs, want the 1 recovered job\nforwarder log:\n%s", resumed, run.logs.String())
		}
		peer.await(t, 1, "the recovered job must reach the peer")
		egAwaitState(t, run.outbox, owedJob, relay.OutboxDelivered, "the recovered job settles exactly once")

		// A repeat Resume is REFUSED. That is the mechanism behind "a job
		// recovered here is offered at most once per process lifetime", and it
		// holds no matter how many callers there are.
		if n, err := run.forwarder.Resume(); !errors.Is(err, relay.ErrForwarderResumed) || n != 0 {
			t.Fatalf("a repeat Resume returned (%d, %v), want (0, relay.ErrForwarderResumed): a second pass would offer every recovered job a second time", n, err)
		}

		// Live traffic AFTER the resume: one send, one durable job, one delivery.
		env, err := egEnvelopeBuilder(t, sender).envelope(live)
		if err != nil {
			t.Fatalf("building the live envelope: %v", err)
		}
		if _, err := run.forwarder.Enqueue(env); err != nil {
			t.Fatalf("live Enqueue after Resume = %v, want it accepted", err)
		}
		peer.await(t, 2, "the live send must reach the peer too")
		peer.quiet(t, 2, "no job may be offered to a peer twice")
		counts := peer.byMessageID()
		for _, m := range []store.Message{owed, live} {
			if counts[m.ID] != 1 {
				t.Fatalf("the peer received message %s %d times, want exactly 1.\n\nA jobID offered twice is a wire duplicate produced by THIS bus, not by the network.\n%s", m.ID, counts[m.ID], egRenderRequests(peer.received()))
			}
		}
		egAwaitState(t, run.outbox, relay.DeriveJobID(egPeerBusID, live.ID), relay.OutboxDelivered, "the live job settles exactly once")
		if n := run.outbox.Len(); n != 2 {
			t.Fatalf("the outbox holds %d records, want 2 (one per message): a second record for one message is the durable half of a double-queue", n)
		}

		// And the correctly-ordered run says nothing about ordering: the warn
		// below belongs to the WRONG order and must not fire here, or it would
		// be noise an operator learns to ignore.
		if strings.Contains(run.logs.String(), "relay forwarding started BEFORE Resume") {
			t.Fatalf("a run that resumed before it forwarded anything logged the out-of-order warning:\n%s", run.logs.String())
		}
	})

	t.Run("an enqueue before resume is WARNED about, and still never doubles the durable job", func(t *testing.T) {
		// THE OTHER ORDERING MISTAKE, and the one the implementation answers
		// with visibility rather than refusal: forwarding that starts before
		// Resume. forward.go declines to refuse the enqueue on purpose — a
		// refusal converts a startup-ordering mistake into a TOTAL forwarding
		// outage, while what it would prevent is a duplicate that invariant 10's
		// applied-key check is designed to absorb.
		//
		// So this test pins what is actually promised, and NOT "the peer sees it
		// once": with the message still owed from the last run, Resume may
		// legitimately offer a second copy of the same job. What may never
		// happen is a SECOND DURABLE JOB for one (peer, message), or a second
		// settlement overwriting the first.
		dir := t.TempDir()
		peer := newEgPeerBus(t)
		sender := egAgentID(t, egLocalBus, "alpha")
		owed := egFixtureMessage(t, 31, sender, egAgentID(t, egPeerBusID, "gamma"), []byte("owed and re-sent"))

		recs := egCrashWithPendingJob(t, dir, owed)
		jobID := recs[0].JobID

		run := egRecover(t, dir, peer, true, owed)
		defer run.close(t)

		env, err := egEnvelopeBuilder(t, sender).envelope(owed)
		if err != nil {
			t.Fatalf("building the envelope: %v", err)
		}
		if _, err := run.forwarder.Enqueue(env); err != nil {
			t.Fatalf("Enqueue before Resume = %v, want it accepted: refusing would turn an ordering mistake into a forwarding outage", err)
		}
		if !strings.Contains(run.logs.String(), "relay forwarding started BEFORE Resume") {
			t.Fatalf(`an Enqueue that ran before Resume logged no warning.

The forwarder accepts it deliberately, so this line is the whole of the control:
it is the only signal that the composition root has its stages out of order, and
without it the duplicate it produces is invisible.

forwarder log:
%s`, run.logs.String())
		}
		if n := run.outbox.Len(); n != 1 {
			t.Fatalf("the outbox holds %d records after a live enqueue for a job it already owed, want 1.\n\nOutbox.Enqueue's idempotent path must hand back the EXISTING record: two records for one (peer, message) is the durable half of a double-queue, and both would be re-offered at every restart.", n)
		}

		if _, err := run.forwarder.Resume(); err != nil {
			t.Fatalf("Forwarder.Resume after an early enqueue = %v", err)
		}
		peer.await(t, 1, "the message must reach the peer at least once")
		egAwaitState(t, run.outbox, jobID, relay.OutboxDelivered, "the job settles delivered")
		rec, _ := run.outbox.Lookup(jobID)
		if rec.State != relay.OutboxDelivered {
			t.Fatalf("job %s ended as %v (reason %q), want delivered: the FIRST settlement stands and a second must change nothing", jobID, rec.State, rec.Reason)
		}
		if n := run.outbox.Len(); n != 1 {
			t.Fatalf("the outbox holds %d records for one message, want 1", n)
		}
	})
}
