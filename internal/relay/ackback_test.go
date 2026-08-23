package relay

// ACK-5 — MULTI-HOP ACK/NACK PROPAGATION AND CORRELATION.
//
// The stored proof command names TestThreeBusAckNackPropagation, and that test
// is a REAL A <- B <- C chain: two live TLS servers, each running the real
// AckHandler, joined by the real Client.PeerAck. Nothing between C and A is
// simulated except the two things a unit test cannot own — the durable
// lifecycle row at A (a recorder that applies relay.DecideAck, the same
// function the production settle path uses) and the intermediate's composition
// at B (a callback that reproduces cmd/agent-bus's federation.disposeUnrecordedAck
// step for step, because that function lives in package main and cannot be
// imported).
//
// # WHAT THE CHAIN IS FOR
//
// A terminal recipient outcome raised at C travels BACKWARDS one hop at a time
// and STOPS at the origin bus A, which is the only bus holding a durable
// sender-visible row (ACK-CONTRACT.md §9.4, §13.3). Correlation across hops is
// by §3's key — A's own server-minted message id — and by NOTHING else. The
// intermediate writes nothing durable, re-signs nothing and re-classifies
// nothing, and the whole thing is SYNCHRONOUS so that no hop answers "accepted"
// before the origin has (invariant 4, end to end through the chain rather than
// through a local write).
//
// # THE FIXTURES THIS FILE DELIBERATELY REUSES
//
// ackTable (the adversarial AckObligations) and ackAgentID come from
// ackhttp_test.go. Everything else here is prefixed abp* so the two files can
// never collide, and so a reader can tell at a glance which bus a helper
// belongs to.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// ---------------------------------------------------------------------------
// Fixtures: the three buses
// ---------------------------------------------------------------------------

const (
	// abpBusA is the ORIGIN: it minted the correlation key and holds the one
	// durable sender-visible row. A terminal outcome must STOP here.
	abpBusA = "abpbusa"
	// abpBusB is the INTERMEDIATE: it relayed the message on and wrote no row,
	// so an acknowledgement reaching it is carried one hop further back.
	abpBusB = "abpbusb"
	// abpBusC is the TERMINAL bus, where the recipient lives and where the
	// terminal outcome is raised.
	abpBusC = "abpbusc"
	// abpBusD is a fourth bus that appears on no legitimate path. It exists so
	// a refusal can name "some other bus" without reusing one of the three.
	abpBusD = "abpbusd"
)

// abpMarker is a distinctive string planted inside an offending value so a test
// can assert the ANTI-ORACLE property directly: a refusal must not echo the hop
// a remote party chose the bytes of. Searching an error for a marker is the
// only form of that assertion that cannot pass by accident.
const abpMarker = "MARKERWORDXQZ"

// abpSignature is the recipient's detached attestation: 64 DISTINCTIVE bytes,
// not a run of one byte, so a hop that truncated, re-signed or zero-filled it
// cannot compare equal by luck. NOBODY VERIFIES IT and nobody may (§6.3) —
// what is under test is that it arrives at A BYTE-IDENTICAL to what C emitted.
func abpSignature() []byte {
	sig := make([]byte, signing.SignatureSize)
	for i := range sig {
		sig[i] = byte(0x11 + i*7)
	}
	return sig
}

// abpKey is the correlation key: the ORIGIN bus's server-minted message id,
// built through the real minter so the fixture cannot drift from the shape
// ids.ParseMessageID enforces (§3, invariant 1).
func abpKey(t testing.TB, seq uint64) string {
	t.Helper()
	id, err := ids.MessageID(abpBusA, seq)
	if err != nil {
		t.Fatalf("ids.MessageID(%q, %d): %v", abpBusA, seq, err)
	}
	return id
}

// abpRecipient is the fully-qualified recipient on the TERMINAL bus
// (invariant 2).
func abpRecipient(t testing.TB) string { return ackAgentID(t, abpBusC, "zulu") }

// abpLogSink captures an operator log so a refusal can be asserted LOUD as well
// as uniform (invariant 6: the silent discard is the defect).
type abpLogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *abpLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *abpLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// ---------------------------------------------------------------------------
// Bus A: the durable lifecycle row, modelled
// ---------------------------------------------------------------------------

// abpOrigin stands in for the ORIGIN bus's ack.Store: a table keyed on
// (correlation key, recipient) — §3.2's per-recipient sub-key — that applies
// the REAL relay.DecideAck, so "exactly once", "a replay is absorbed" and "a
// conflict is refused" are decided by production code rather than by a
// convenience the test invented.
//
// It counts separately, because the whole of point 1 is a COUNTER and not an
// inference: settles is every call, applied is every FIRST terminal actually
// written, replays is invariant 10's first case and conflicts its second.
type abpOrigin struct {
	mu        sync.Mutex
	rows      map[string]AckTerminal
	frames    []ValidatedPeerAck
	peers     []string
	settles   int
	applied   int
	replays   int
	conflicts int
}

func newABPOrigin() *abpOrigin { return &abpOrigin{rows: map[string]AckTerminal{}} }

// abpRowKey is §3.2's (correlation key, recipient) pair. The NUL separator
// makes it unambiguous for ids that may themselves contain '-' and '.'.
func abpRowKey(key, recipient string) string { return key + "\x00" + recipient }

func (o *abpOrigin) settle(_ context.Context, s SettledAck) (AckSettlement, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.settles++
	o.frames = append(o.frames, s.Ack)
	o.peers = append(o.peers, s.PeerBusID)

	rk := abpRowKey(s.Ack.CorrelationKey, s.Ack.Recipient)
	prior, hasPrior := o.rows[rk]
	decision, err := DecideAck(prior, hasPrior, s.Ack.Terminal())
	switch {
	case err != nil:
		o.conflicts++
		return AckSettlement{}, err
	case decision == AckApply:
		o.rows[rk] = s.Ack.Terminal()
		o.applied++
		return AckSettlement{Duplicate: false}, nil
	case decision == AckReplay:
		o.replays++
		// Invariant 10's FIRST case: the ORIGINAL result stands, nothing is
		// re-applied, no error and no disconnect.
		return AckSettlement{Duplicate: true}, nil
	default:
		return AckSettlement{}, fmt.Errorf("abpOrigin: unexpected decision %s", decision)
	}
}

func (o *abpOrigin) snapshot() (settles, applied, replays, conflicts int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.settles, o.applied, o.replays, o.conflicts
}

func (o *abpOrigin) terminal(key, recipient string) (AckTerminal, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	tm, ok := o.rows[abpRowKey(key, recipient)]
	return tm, ok
}

func (o *abpOrigin) lastFrame() (ValidatedPeerAck, string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.frames) == 0 {
		return ValidatedPeerAck{}, "", false
	}
	return o.frames[len(o.frames)-1], o.peers[len(o.peers)-1], true
}

// ---------------------------------------------------------------------------
// Bus B: the emitting half, with a COUNTING sender in front of the real client
// ---------------------------------------------------------------------------

// abpSender is an AckSender that COUNTS before delegating.
//
// It is the instrument for the one property that cannot be observed any other
// way: "nothing is dialled". BackPropagator.Propagate resolves the address
// FIRST and only then calls the sender, so a refusal that never touches this
// counter is a refusal that opened no socket. A test that only asserted the
// error would pass just as well against an implementation that dialled and then
// threw the answer away.
type abpSender struct {
	inner  AckSender
	mu     sync.Mutex
	calls  int
	urls   []string
	frames []PeerAckRequest
}

func (s *abpSender) PeerAck(ctx context.Context, peerBaseURL string, req PeerAckRequest) (PeerAckResponse, error) {
	s.mu.Lock()
	s.calls++
	s.urls = append(s.urls, peerBaseURL)
	s.frames = append(s.frames, req)
	s.mu.Unlock()
	return s.inner.PeerAck(ctx, peerBaseURL, req)
}

func (s *abpSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *abpSender) lastFrame() (PeerAckRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.frames) == 0 {
		return PeerAckRequest{}, false
	}
	return s.frames[len(s.frames)-1], true
}

// ---------------------------------------------------------------------------
// The chain
// ---------------------------------------------------------------------------

// abpChain is A <- B <- C wired end to end.
type abpChain struct {
	key       string
	recipient string

	srvA *httptest.Server
	srvB *httptest.Server

	origin *abpOrigin
	prop   *BackPropagator
	sender *abpSender
	client *Client

	tableA *ackTable
	tableB *ackTable

	logsA *abpLogSink
	logsB *abpLogSink

	// peered is B's peer registry, reduced to the one question PeerBaseURL
	// answers. Atomic because the handler goroutine reads it.
	peered atomic.Bool

	// bodyAtA is the RAW request body A received, captured before the handler
	// reads it. Raw bytes are the only way to assert that an ABSENT class is
	// absent: `omitempty` means a wrongly-copied zero class would produce a key
	// the decoded struct cannot tell from a legitimately absent one.
	bodyMu   sync.Mutex
	bodyAtA  [][]byte
	connsToA atomic.Int64
	connsToB atomic.Int64

	// forwarded and finalRefusals are B's own dispositions: how many terminal
	// outcomes it carried one hop back, and how many the upstream FINALLY
	// refused (which B answers downstream as success — see abpIntermediateSettle).
	forwarded     atomic.Int64
	finalRefusals atomic.Int64
}

// newABPChain builds the whole chain. The construction order is forced by the
// topology: A's address must exist before B can be told how to reach it, and
// B's address must exist before C can emit into it.
func newABPChain(t *testing.T, seq uint64) *abpChain {
	t.Helper()
	c := &abpChain{
		key:       abpKey(t, seq),
		recipient: abpRecipient(t),
		origin:    newABPOrigin(),
		tableA:    newAckTable(),
		tableB:    newAckTable(),
		logsA:     &abpLogSink{},
		logsB:     &abpLogSink{},
	}
	c.peered.Store(true)

	// THE OBLIGATIONS, which are what §6.2 binds each hop on. The outbox job is
	// keyed on the RECIPIENT'S HOME BUS (Forwarder.targets -> Registry.Route,
	// forward.go:886), NOT on the next hop dialled. The recipient lives on C, so
	// BOTH A and B key their job for it on C: A reaches C through its peer B (the
	// nextHop wired into handlerA below), and B reaches C directly. At A the ACK
	// therefore arrives from peer B while the job is keyed on C — the DIRECT arm
	// MISSES and the INDIRECT arm binds it (this is the multi-hop shape ACK-5's
	// indirect arm exists for; the pre-ACK-4-FU keying on abpBusB let the direct
	// arm hit at A for a recipient it was never routed, which is exactly the
	// binding ACK-4-FU-RECIPIENT-BINDING closes). Neither bus owes anything to
	// anybody else, which is what makes the forgery arms of AuthorizePeerAck live.
	c.tableA.owe(abpBusC, c.key)
	c.tableB.owe(abpBusC, c.key)

	// ----- Bus A: the origin, with the durable row behind it. -----
	handlerA, err := NewAckHandler(AckConfig{
		BusID:       abpBusA,
		Obligations: c.tableA,
		Admit:       func(string) (func(), error) { return func() {}, nil },
		SettleAck:   c.origin.settle,
		// A routes the recipient's home bus C through its peer B, so both resolve
		// to the same dial address — the INDIRECT arm's binding question. This is
		// A's OWN routing table (Registry.PeerBaseURL in production), never
		// anything the frame carries. The address value is irrelevant; only the
		// EQUALITY of the two answers is, and both being non-empty.
		NextHopAddress: func(busID string) (string, bool) {
			if strings.EqualFold(busID, abpBusB) || strings.EqualFold(busID, abpBusC) {
				return "https://hop-b.abp.invalid", true
			}
			return "", false
		},
		Logger: logging.New(c.logsA, logging.LevelDebug),
	})
	if err != nil {
		t.Fatalf("NewAckHandler(bus A): %v", err)
	}
	srvA := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, MaxAckBytes+1))
		r.Body = io.NopCloser(bytes.NewReader(raw))
		c.bodyMu.Lock()
		c.bodyAtA = append(c.bodyAtA, raw)
		c.bodyMu.Unlock()
		// THE AUTHENTICATED PEER IS B AND IS A GO PARAMETER, exactly as
		// RequirePeerPrincipal supplies it in production. Nothing in the frame
		// could name it, and this mount could not read it from there if it did.
		handlerA.ServeAuthenticated(w, r, abpBusB)
	}))
	// http.StateNew counts SOCKETS OPENED, which is how "nobody was
	// disconnected" is asserted as a fact about a connection rather than about
	// a status code: Go's transport transparently redials, so a chain that hung
	// up after every refusal would pass a status-only test perfectly.
	srvA.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			c.connsToA.Add(1)
		}
	}
	srvA.StartTLS()
	t.Cleanup(srvA.Close)
	c.srvA = srvA

	// ----- Bus B: the intermediate. It holds no row and writes nothing. -----
	c.sender = &abpSender{inner: &Client{
		busID:      abpBusB,
		httpClient: srvA.Client(),
		log:        logging.New(io.Discard, logging.LevelError),
	}}
	c.prop, err = NewBackPropagator(BackPropagatorConfig{
		BusID:  abpBusB,
		Sender: c.sender,
		// THE ONE SOURCE OF AN ADDRESS ON THIS PATH (§9.4). It answers only for
		// A, and only while B considers A a peer — de-peering is a flag flip.
		PeerBaseURL: func(busID string) (string, bool) {
			if strings.EqualFold(busID, abpBusA) && c.peered.Load() {
				return srvA.URL, true
			}
			return "", false
		},
		Logger: logging.New(c.logsB, logging.LevelDebug),
	})
	if err != nil {
		t.Fatalf("NewBackPropagator(bus B): %v", err)
	}
	handlerB, err := NewAckHandler(AckConfig{
		BusID:       abpBusB,
		Obligations: c.tableB,
		Admit:       func(string) (func(), error) { return func() {}, nil },
		SettleAck:   c.intermediateSettle,
		Logger:      logging.New(c.logsB, logging.LevelDebug),
	})
	if err != nil {
		t.Fatalf("NewAckHandler(bus B): %v", err)
	}
	srvB := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerB.ServeAuthenticated(w, r, abpBusC)
	}))
	srvB.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			c.connsToB.Add(1)
		}
	}
	srvB.StartTLS()
	t.Cleanup(srvB.Close)
	c.srvB = srvB

	// ----- Bus C: the emitter. The real client, over real TLS. -----
	c.client = &Client{
		busID:      abpBusC,
		httpClient: srvB.Client(),
		log:        logging.New(io.Discard, logging.LevelError),
	}
	return c
}

// storedPath is what hub.relayedBusPath wrote on bus B: ORIGIN-FIRST and ENDING
// AT B. It is NOT the path as it arrived on the wire, and handing the wire path
// to UpstreamHop is the steering hole that function's doc describes.
func (c *abpChain) storedPath() []string { return []string{abpBusA, abpBusB} }

// intermediateSettle reproduces cmd/agent-bus's federation.disposeUnrecordedAck
// followed by ackTransit.TransitAck, step for step. It is duplicated rather than
// imported because both live in package main; TestSettleAckDisposition in
// cmd/agent-bus/acktransit_test.go drives the real ones, and this models the
// SHAPE so the chain here is honest about what production composes.
func (c *abpChain) intermediateSettle(ctx context.Context, s SettledAck) (AckSettlement, error) {
	// 1. THE ORIGIN TEST, DisposeAck's own, WITH A NIL PATH — "are we the bus
	//    that minted this key". At the origin a missing row is §8.2's "(none)"
	//    row and the answer is the UNIFORM refusal, unchanged.
	if disp, _, err := DisposeAck(abpBusB, s.Ack.CorrelationKey, nil); err == nil && disp == AckStopAtOrigin {
		return AckSettlement{}, ErrAckNotBound
	}

	// 2. THE STORED PATH, from THIS bus's own retained provenance. Nothing in
	//    the frame names an address, a host or a destination bus.
	disp, upstream, err := DisposeAck(abpBusB, s.Ack.CorrelationKey, c.storedPath())
	if err != nil {
		return AckSettlement{}, err
	}
	if disp == AckStopAtOrigin {
		return AckSettlement{}, errors.New("abpChain: the intermediate disposed its own key as origin; wiring fault")
	}

	// 3. THE EMISSION, with the frame rebuilt from the VALIDATED value and
	//    forwarded VERBATIM.
	if _, err := c.prop.Propagate(ctx, upstream, AckFrameFrom(s.Ack)); err != nil {
		var refused *PeerRefusedError
		// A 409 — AND NOTHING ELSE — is answered DOWNSTREAM AS SUCCESS. It is
		// the one refusal that means the upstream UNDERSTOOD the frame and
		// DECIDED about it, and absorbing it is the deliberate anti-oracle
		// choice disposeUnrecordedAck documents: re-offering a frame the origin
		// has finally refused is the retry amplification §9.3 exists to stop,
		// and forwarding the origin's 409 verbatim would tell any bound peer
		// whether the ORIGIN holds a row for a recipient it named.
		//
		// The test is the STATUS, never Retriable(): Retriable() calls every
		// 4xx except 408/429 final, so switching on it would sweep 404, 403 and
		// 400 — each an upstream that decided NOTHING — into a 200 the
		// recipient reads as "accepted" for an outcome nobody recorded.
		if errors.As(err, &refused) && refused.StatusCode == http.StatusConflict {
			c.finalRefusals.Add(1)
			return AckSettlement{Duplicate: false}, nil
		}
		return AckSettlement{}, err
	}
	c.forwarded.Add(1)
	return AckSettlement{Duplicate: false}, nil
}

// emit is bus C raising a terminal outcome: the REAL Client.PeerAck over real
// TLS into B's real AckHandler.
func (c *abpChain) emit(ctx context.Context, frame PeerAckRequest) (PeerAckResponse, error) {
	return c.client.PeerAck(ctx, c.srvB.URL, frame)
}

// abpFrame is the frame C emits. ProtocolVersion is left ZERO on purpose:
// Client.PeerAck stamps it, which is what makes an absent version on the wire
// unambiguously mean "written before the field existed".
func abpFrame(key, recipient string, outcome AckOutcome, class AckClass) PeerAckRequest {
	f := PeerAckRequest{
		CorrelationKey:     key,
		Recipient:          recipient,
		Outcome:            outcome.String(),
		EmittedAtUnixMilli: 1_755_000_000_123,
	}
	f.Class = string(class)
	if outcome.RecipientSourced() {
		f.Attestation = &AckAttestationEnvelope{Signature: abpSignature()}
	}
	return f
}

// ---------------------------------------------------------------------------
// THE PROOF
// ---------------------------------------------------------------------------

// TestThreeBusAckNackPropagation is ACK-5's stored proof command.
//
// Every subtest ASSERTS; none skips. The properties, in the order the chain
// establishes them:
//
//  1. end to end, EXACTLY ONCE, under the key C used;
//  2. VERBATIM — class, attestation bytes, recipient and emitter clock;
//  3. §8.4 — a hop receipt is not a delivery, at any distance;
//  4. a replay is absorbed, a conflict is not, and NOBODY is disconnected;
//  5. a de-peered upstream drops LOUDLY and dials NOTHING;
//  6. the origin does not re-emit, so an outcome cannot orbit the federation.
func TestThreeBusAckNackPropagation(t *testing.T) {
	t.Run("a terminal raised at C reaches A exactly once and verbatim", testABPEndToEnd)
	t.Run("a hop receipt is NOT a delivery (§8.4)", testABPHopReceiptIsNotDelivery)
	t.Run("a replay is absorbed and a conflict is refused, with no disconnect", testABPIdempotency)
	t.Run("a de-peered upstream drops loudly and dials nothing", testABPDePeeredUpstream)
	t.Run("the origin does not re-emit", testABPOriginDoesNotReEmit)
}

// testABPEndToEnd is points 1 and 2, table-driven over the positive terminal and
// a negative one, because the class is the field an intermediate is most likely
// to re-derive and a positive terminal carries NONE at all (§5.4).
func testABPEndToEnd(t *testing.T) {
	sig := abpSignature()
	for _, tc := range []struct {
		name    string
		outcome AckOutcome
		class   AckClass
		seq     uint64
	}{
		{"a positive terminal carries no class", AckDelivered, ackNoClass, 11},
		{"a negative terminal carries its recipient-emitted class", AckRefused, AckRecipientRefusedPolicy, 12},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := newABPChain(t, tc.seq)
			frame := abpFrame(c.key, c.recipient, tc.outcome, tc.class)

			resp, err := c.emit(context.Background(), frame)
			if err != nil {
				t.Fatalf("C -> B -> A: %v", err)
			}
			if !resp.Accepted || resp.Duplicate {
				t.Fatalf("C got %+v, want accepted:true duplicate:false", resp)
			}

			// ---- EXACTLY ONCE, AS A COUNTER. -----------------------------
			settles, applied, replays, conflicts := c.origin.snapshot()
			if settles != 1 || applied != 1 || replays != 0 || conflicts != 0 {
				t.Fatalf("the origin saw settles=%d applied=%d replays=%d conflicts=%d, want 1/1/0/0 — one terminal outcome raised at C must land at A exactly ONCE",
					settles, applied, replays, conflicts)
			}
			if got := c.forwarded.Load(); got != 1 {
				t.Errorf("the intermediate forwarded %d times, want 1", got)
			}
			if got := c.prop.Stats(); got.Forwarded != 1 || got.Failed != 0 || got.NotPeered != 0 {
				t.Errorf("BackPropagator.Stats() = %+v, want Forwarded:1 Failed:0 NotPeered:0", got)
			}

			// ---- UNDER THE SAME CORRELATION KEY C USED. ------------------
			got, peer, ok := c.origin.lastFrame()
			if !ok {
				t.Fatal("nothing reached the origin's durable settle")
			}
			if got.CorrelationKey != c.key {
				t.Fatalf("the origin recorded correlation key %q, want %q. §3's key is the ORIGIN bus's own message id and there is no fourth identifier; an intermediate that rewrote it would make the row unfindable by the sender's status read",
					got.CorrelationKey, c.key)
			}
			if _, ok := c.origin.terminal(c.key, c.recipient); !ok {
				t.Fatalf("no durable row at the origin for (%q,%q)", c.key, c.recipient)
			}
			// And it arrived from B, one hop, never from C directly: a bus
			// contacts only the bus that handed it the message (§9.4).
			if peer != abpBusB {
				t.Errorf("the origin's settle saw peer %q, want %q — nothing on this path may skip a hop to reach the origin directly", peer, abpBusB)
			}

			// ---- VERBATIM. ----------------------------------------------
			if got.Recipient != c.recipient {
				t.Errorf("recipient = %q, want %q", got.Recipient, c.recipient)
			}
			if got.EmittedAtUnixMilli != frame.EmittedAtUnixMilli {
				t.Errorf("emitted_at = %d, want %d — it is the EMITTER's clock and stays the emitter's clock; restamping it would make every hop look like the origin of the outcome",
					got.EmittedAtUnixMilli, frame.EmittedAtUnixMilli)
			}
			if got.Outcome != tc.outcome {
				t.Errorf("outcome = %s, want %s", got.Outcome, tc.outcome)
			}
			if got.Class != tc.class {
				t.Errorf("class = %s, want %s — an intermediate re-classifies NOTHING (§9.4)", got.Class, tc.class)
			}
			if !bytes.Equal(got.Signature, sig) {
				t.Errorf("the attestation signature that reached the origin is %x, want %x. The attestation is opaque bytes over the ORIGINAL canonical ACK bytes: touching a covered field would silently invalidate a signature nobody in this federation can verify yet, so the corruption would be undetectable end to end (§6.3)",
					got.Signature, sig)
			}
			if got.Attestation != AckAttestedRecipientSignatureUnverified {
				t.Errorf("attestation label = %s, want recipient_signature_unverified; there is no label meaning verified and one must not be added", got.Attestation)
			}

			// ---- B DID NOT ALTER THE KEY ON THE WIRE EITHER. -------------
			sent, ok := c.sender.lastFrame()
			if !ok {
				t.Fatal("the intermediate's sender was never called")
			}
			if sent.CorrelationKey != c.key || sent.Recipient != c.recipient {
				t.Errorf("the intermediate emitted (%q,%q), want (%q,%q)", sent.CorrelationKey, sent.Recipient, c.key, c.recipient)
			}
			if !bytes.Equal(sent.Attestation.Signature, sig) {
				t.Error("the intermediate re-signed or reshaped the attestation before forwarding it")
			}

			// ---- THE ABSENT CLASS IS ABSENT ON THE WIRE. -----------------
			//
			// Asserted from the RAW bytes A received, not from the decoded
			// struct: `omitempty` means a wrongly-copied zero class would be
			// encoded as "AckClass(0)" — a spelling ParseAckClass refuses — and
			// the next hop would answer 400, which is FINAL, losing the outcome
			// rather than retrying it.
			c.bodyMu.Lock()
			bodies := append([][]byte(nil), c.bodyAtA...)
			c.bodyMu.Unlock()
			if len(bodies) != 1 {
				t.Fatalf("the origin received %d request bodies, want 1", len(bodies))
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(bodies[0], &raw); err != nil {
				t.Fatalf("the body the intermediate sent is not an object: %v (%s)", err, bodies[0])
			}
			rawClass, present := raw["class"]
			if tc.class == ackNoClass {
				if present {
					t.Errorf("a POSITIVE terminal carried class=%s on the wire; §5.4 says it carries no class at all", rawClass)
				}
			} else {
				if !present {
					t.Fatalf("a NEGATIVE terminal carried NO class on the wire (body %s); the class is the recipient's own declaration and is forwarded verbatim", bodies[0])
				}
				if string(rawClass) != fmt.Sprintf("%q", tc.class.String()) {
					t.Errorf("class on the wire = %s, want %q", rawClass, tc.class.String())
				}
			}
		})
	}
}

// testABPHopReceiptIsNotDelivery is §8.4 at the correlation layer: the 200 B
// gives C settles B's HOP obligation and moves the origin's sender-visible state
// NOT AT ALL.
//
// The shape is chosen so the two facts are simultaneously true and could not be
// confused: the origin FINALLY refuses (409, no obligation binds B for this
// key), B answers C 200 anyway, and A's row table is empty.
func testABPHopReceiptIsNotDelivery(t *testing.T) {
	c := newABPChain(t, 21)

	// A NO LONGER BINDS THIS KEY TO B — the obligation was swept, or was never
	// written for this route. AuthorizePeerAck answers ErrAckNotBound, which
	// the route maps onto a 409, which is FINAL.
	//
	// Taken under the table's own mutex because the handler goroutine reads it
	// there; concurrency here is the product and a data race is a P0.
	c.tableA.mu.Lock()
	c.tableA.jobs = map[string]OutboxRecord{}
	c.tableA.mu.Unlock()

	resp, err := c.emit(context.Background(), abpFrame(c.key, c.recipient, AckDelivered, ackNoClass))
	if err != nil {
		t.Fatalf("C -> B: %v — a FINAL refusal upstream must not be reported downstream as a transport failure", err)
	}

	// B ANSWERED 200.
	if !resp.Accepted {
		t.Fatalf("C got %+v, want accepted:true. A frame the ORIGIN has FINALLY refused is answered 200 downstream on purpose: re-offering it is the retry amplification §9.3 exists to stop, and forwarding the origin's 409 verbatim would turn this hop into an oracle for whether the origin holds a row for a named recipient",
			resp)
	}
	if got := c.finalRefusals.Load(); got != 1 {
		t.Fatalf("the intermediate recorded %d final refusals, want 1; the fixture did not reach the arm it claims to test", got)
	}

	// AND THE ORIGIN RECORDED NOTHING. This is the whole of §8.4: B's 200 is
	// not, and can never become, a delivery at A.
	if _, ok := c.origin.terminal(c.key, c.recipient); ok {
		t.Fatal("the origin holds a terminal row after refusing the frame; a hop receipt has been converted into a delivery")
	}
	if _, _, _, conflicts := c.origin.snapshot(); conflicts != 0 {
		t.Errorf("the origin recorded %d conflicts; the refusal came from the binding rule, before any settle", conflicts)
	}
	if settles, applied, _, _ := c.origin.snapshot(); settles != 0 || applied != 0 {
		t.Errorf("the origin's settle ran %d times and applied %d; AuthorizePeerAck must refuse before anything durable is touched", settles, applied)
	}

	// The drop is LOUD (invariant 6): uniform on the wire must never mean
	// invisible to an operator.
	if logs := c.logsB.String(); !strings.Contains(logs, "NOT propagated") {
		t.Errorf("the intermediate did not log the failed hop loudly; log was:\n%s", logs)
	}
}

// testABPIdempotency is invariant 10's first two cases carried across two hops,
// plus the rule that neither of them disconnects anybody (§12).
func testABPIdempotency(t *testing.T) {
	c := newABPChain(t, 31)
	ctx := context.Background()
	frame := abpFrame(c.key, c.recipient, AckDelivered, ackNoClass)

	if _, err := c.emit(ctx, frame); err != nil {
		t.Fatalf("first emission: %v", err)
	}

	// ---- THE IDENTICAL REPLAY. --------------------------------------------
	// Invariant 10's FIRST case: the original result stands, nothing is
	// re-applied, it does not error and it does not disconnect.
	resp, err := c.emit(ctx, frame)
	if err != nil {
		t.Fatalf("the identical replay errored: %v — a legitimate retry must be absorbed, not punished", err)
	}
	if !resp.Accepted {
		t.Errorf("the identical replay was answered %+v, want accepted:true", resp)
	}
	settles, applied, replays, conflicts := c.origin.snapshot()
	if applied != 1 {
		t.Errorf("the origin APPLIED %d terminals over one outcome and its replay, want 1", applied)
	}
	if replays != 1 || settles != 2 || conflicts != 0 {
		t.Errorf("the origin saw settles=%d applied=%d replays=%d conflicts=%d, want 2/1/1/0", settles, applied, replays, conflicts)
	}
	if tm, ok := c.origin.terminal(c.key, c.recipient); !ok || tm.Outcome != AckDelivered {
		t.Fatalf("after the replay the origin holds (%+v,%v), want delivered", tm, ok)
	}

	// ---- A DIFFERENT TERMINAL FOR THE SAME PAIR. --------------------------
	// Invariant 10's SECOND case: reject and log, do NOT disconnect, and
	// terminal is ABSORBING — the FIRST one stands.
	conflicting := abpFrame(c.key, c.recipient, AckRefused, AckRecipientRefusedPolicy)
	if _, err := c.emit(ctx, conflicting); err != nil {
		// The chain answers C 200 here by design (the upstream 409 is FINAL and
		// is not echoed downstream); a transport error would mean something
		// closed the socket.
		t.Fatalf("the conflicting terminal produced a transport-level error: %v", err)
	}
	settles, applied, replays, conflicts = c.origin.snapshot()
	if conflicts != 1 {
		t.Errorf("the origin recorded %d conflicts, want 1", conflicts)
	}
	if applied != 1 {
		t.Errorf("the origin APPLIED %d terminals, want 1 — terminal is ABSORBING and is never revisited, reopened or downgraded", applied)
	}
	tm, ok := c.origin.terminal(c.key, c.recipient)
	if !ok || tm.Outcome != AckDelivered || tm.Class != ackNoClass {
		t.Fatalf("after the conflict the origin holds (%+v,%v), want the FIRST terminal delivered/none still standing", tm, ok)
	}
	// AND B OVERWROTE NOTHING: it holds no row at all, by construction, and its
	// only durable-looking state is a counter.
	if got := c.forwarded.Load(); got != 2 {
		t.Errorf("the intermediate forwarded %d outcomes, want 2 (the original and its replay); the conflicting third was FINALLY refused upstream", got)
	}
	if got := c.finalRefusals.Load(); got != 1 {
		t.Errorf("the intermediate recorded %d final refusals, want 1", got)
	}

	// ---- NOBODY WAS DISCONNECTED. -----------------------------------------
	//
	// Asserted twice over: a further request must SUCCEED, and the socket
	// counters must show it landed on the connection already open. Go's
	// transport transparently redials, so "the next request worked" alone is
	// not evidence — a hop that hung up after every refusal would pass it.
	beforeA, beforeB := c.connsToA.Load(), c.connsToB.Load()
	if beforeA < 1 || beforeB < 1 {
		t.Fatalf("only %d/%d connections were ever observed to A/B; the reuse assertion would be vacuous", beforeA, beforeB)
	}
	if _, err := c.emit(ctx, frame); err != nil {
		t.Fatalf("after a replay and a conflict the C->B connection was unusable: %v. NO ACK-plane refusal may disconnect (§12, invariant 10)", err)
	}
	if got := c.connsToB.Load(); got != beforeB {
		t.Errorf("C opened a NEW connection to B (%d -> %d): the intermediate dropped the socket over a refusal, and a peer link carries every agent behind it",
			beforeB, got)
	}
	if got := c.connsToA.Load(); got != beforeA {
		t.Errorf("B opened a NEW connection to A (%d -> %d): the origin dropped the socket over a refusal", beforeA, got)
	}
	if _, _, replays, _ = c.origin.snapshot(); replays != 2 {
		t.Errorf("the origin absorbed %d replays, want 2", replays)
	}
}

// testABPDePeeredUpstream is point 5: the neighbour is gone, the drop is LOUD,
// and NOTHING IS DIALLED.
//
// §9.4 makes "not peered" the end of the road for the hop — never a reason to
// look for another route to the origin — so the absence of a dial is the
// property, not the error text.
func testABPDePeeredUpstream(t *testing.T) {
	c := newABPChain(t, 41)
	c.peered.Store(false)

	// ---- DIRECTLY, so the sentinel and the counters are unambiguous. ------
	_, err := c.prop.Propagate(context.Background(), abpBusA, abpFrame(c.key, c.recipient, AckDelivered, ackNoClass))

	// THE NO-DIAL ASSERTION COMES FIRST, AND ON PURPOSE. It is the property
	// this subtest exists for, and every check below it is Errorf rather than
	// Fatalf so a change that starts dialling — which also changes the error —
	// cannot stop the run before the counter is read. An earlier ordering hid
	// exactly that: the sentinel assertion fatally failed and the dial went
	// unobserved.
	if got := c.sender.count(); got != 0 {
		t.Fatalf("the sender was called %d times for a bus that is not in the peer registry. A bus contacts ONLY a bus it is peered with (§9.4), and there is no fallback address to try", got)
	}
	if got := c.connsToA.Load(); got != 0 {
		t.Errorf("%d connections were opened to the de-peered upstream", got)
	}

	if !errors.Is(err, ErrUpstreamNotPeered) {
		t.Errorf("Propagate to a de-peered upstream = %v, want ErrUpstreamNotPeered. It is a SEPARATE sentinel from ErrNoUpstreamHop on purpose: this one means a neighbour was de-peered while messages it relayed are still settling, and is fixed by re-peering",
			err)
	}
	if errors.Is(err, ErrNoUpstreamHop) {
		t.Error("a de-peered neighbour was reported as ErrNoUpstreamHop; folding the two would send an operator hunting a path-fabrication bug over a peer they removed on purpose")
	}
	stats := c.prop.Stats()
	if stats.NotPeered != 1 {
		t.Errorf("Stats().NotPeered = %d, want 1", stats.NotPeered)
	}
	if stats.Failed != 1 {
		t.Errorf("Stats().Failed = %d, want 1 — Failed is every emission that did not forward, this subset included", stats.Failed)
	}
	if stats.Forwarded != 0 {
		t.Errorf("Stats().Forwarded = %d, want 0", stats.Forwarded)
	}

	// ---- LOUD AND SPECIFIC (invariant 6). --------------------------------
	logs := c.logsB.String()
	for _, want := range []string{"NOT propagated", "NOT a peer", "NOTHING was dialled", c.key} {
		if !strings.Contains(logs, want) {
			t.Errorf("the operator log does not name %q; a terminal outcome is being dropped and a discard nobody can diagnose is the actual defect. Log was:\n%s", want, logs)
		}
	}

	// ---- AND END TO END: C is told "not now" and A learns nothing. --------
	_, err = c.emit(context.Background(), abpFrame(c.key, c.recipient, AckDelivered, ackNoClass))
	var refused *PeerRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("C got %v (%T), want *PeerRefusedError", err, err)
	}
	if refused.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("C got status %d, want 503; nothing was written anywhere, so the identical retry is safe and a FINAL 4xx would make the outcome be abandoned instead of delayed", refused.StatusCode)
	}
	if !refused.Retriable() {
		t.Error("the answer to C reported itself non-retriable; the outcome would be abandoned")
	}
	if settles, applied, _, _ := c.origin.snapshot(); settles != 0 || applied != 0 {
		t.Errorf("the origin saw settles=%d applied=%d, want 0/0", settles, applied)
	}
}

// testABPOriginDoesNotReEmit is point 6, and it is the terminating condition of
// the whole mechanism: the ONE bus that could turn an inbound acknowledgement
// back into an outbound one is the bus that minted the key, and it never does.
func testABPOriginDoesNotReEmit(t *testing.T) {
	c := newABPChain(t, 51)

	// A disposing ITS OWN key stops, whatever the path says.
	disp, upstream, err := DisposeAck(abpBusA, c.key, c.storedPath())
	if err != nil {
		t.Fatalf("DisposeAck at the origin: %v", err)
	}
	if disp != AckStopAtOrigin {
		t.Fatalf("DisposeAck at the origin = %s (upstream %q), want stop_at_origin. If the origin forwarded, a terminal outcome would orbit the federation: idempotency would absorb the duplicate settlements but the TRAFFIC is unbounded (§8.4, invariant 10)",
			disp, upstream)
	}
	if upstream != "" {
		t.Errorf("DisposeAck returned upstream %q alongside stop_at_origin; there is nowhere for an origin's own outcome to go", upstream)
	}

	// And a full end-to-end pass leaves the origin having emitted NOTHING: the
	// only sender on this chain is the intermediate's, and it was called once.
	if _, err := c.emit(context.Background(), abpFrame(c.key, c.recipient, AckDelivered, ackNoClass)); err != nil {
		t.Fatalf("C -> B -> A: %v", err)
	}
	if got := c.sender.count(); got != 1 {
		t.Errorf("the chain made %d onward emissions for one terminal outcome, want exactly 1 (B -> A). More than one means a hop re-emitted", got)
	}
}

// ---------------------------------------------------------------------------
// UpstreamHop — every refusal, and the anti-oracle property
// ---------------------------------------------------------------------------

// TestUpstreamHopRefusals walks EVERY refusal UpstreamHop can make.
//
// Each case asserts the sentinel AND that the returned hop is EMPTY: a function
// that returned a plausible bus id alongside an error would be a function whose
// caller could ignore the error and still dial somebody.
//
// # THE ANTI-ORACLE COLUMN
//
// A hop on a stored path is derived from bytes a PEER sent, so a refusal that
// echoed one would let a peer size — and populate — this bus's operator log.
// Every case plants abpMarker inside the offending value and requires it to be
// ABSENT from the error text.
//
// ONE CASE IS EXEMPTED, AND THE EXEMPTION IS A FINDING RATHER THAN A
// CONVENIENCE: the "malformed hop" arm wraps ids.ValidateBusID's own error,
// which quotes what it rejects with %q. So a malformed hop WITHIN the 64-byte
// bound IS echoed, and only the OVERSIZE case is caught before ValidateBusID
// ever sees the value — which is what UpstreamHop's doc comment actually
// promises when read closely, though its summary sentence ("the refusals below
// report the INDEX and the LENGTH") reads wider than the code delivers. That
// case therefore asserts the property that IS true and is load-bearing: the echo
// is BOUNDED, so a remote party cannot choose the size of the line.
func TestUpstreamHopRefusals(t *testing.T) {
	us := abpBusB
	oversize := abpMarker + strings.Repeat("q", MaxPeerBusIDLen)
	malformed := abpMarker + ".not-a-bus-id"
	markerHop := "zz" + abpMarker + "zz"

	for _, tc := range []struct {
		name       string
		local      string
		path       []string
		markerFree bool
	}{
		{
			// OUR OWN id, checked first: every comparison below measures a hop
			// against this string, so a bad one makes all of them vacuous.
			name:       "this bus's own id is not valid",
			local:      "bad.local.id",
			path:       []string{markerHop, us},
			markerFree: true,
		},
		{
			name:       "a nil path",
			local:      us,
			path:       nil,
			markerFree: true,
		},
		{
			// A one-hop stored path is [us], which says the message ORIGINATED
			// here — and an origin never forwards a terminal outcome.
			name:       "fewer than two hops",
			local:      us,
			path:       []string{markerHop},
			markerFree: true,
		},
		{
			name:       "an oversize hop is refused BEFORE it is validated, and never echoed",
			local:      us,
			path:       []string{oversize, us},
			markerFree: true,
		},
		{
			name:  "a malformed hop",
			local: us,
			path:  []string{malformed, us},
			// EXEMPT — see the doc comment above. The value is echoed by
			// ids.ValidateBusID, bounded to MaxPeerBusIDLen bytes.
			markerFree: false,
		},
		{
			// THE STEERING CHECK, in the shape that actually distinguishes it
			// from a search. Our id IS on this path, in the MIDDLE, and the
			// path ends somewhere else. UpstreamHop must refuse — because the
			// obvious "robustness" generalisation (find our id ANYWHERE and
			// take the hop before it) would return markerHop here, a bus a PEER
			// put on a path it fabricated, and this bus would then POST a
			// terminal settlement to a bus that was never on the route.
			// Requiring the path to END at us means the only hop we ever
			// contact is the one adjacent to a position WE wrote.
			name:       "our id is in the MIDDLE and the path ends elsewhere",
			local:      us,
			path:       []string{markerHop, us, abpBusD},
			markerFree: true,
		},
		{
			name:       "a path that does not name us at all",
			local:      us,
			path:       []string{markerHop, abpBusD},
			markerFree: true,
		},
		{
			name:       "our id appears at a non-final index as well as the last",
			local:      us,
			path:       []string{us, markerHop, us},
			markerFree: true,
		},
		{
			// An adjacent self-pair. It is caught by the "exactly once" rule
			// (step 4) rather than by the post-condition assertion (step 5),
			// which UpstreamHop's own doc calls unreachable after step 4 — both
			// wrap the same sentinel, which is what this case pins.
			name:       "an adjacent self-pair",
			local:      us,
			path:       []string{markerHop, us, us},
			markerFree: true,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			hop, err := UpstreamHop(tc.local, tc.path)
			if err == nil {
				t.Fatalf("UpstreamHop(%q, %v) = %q, nil; this path supports no hop and must be refused rather than guessed", tc.local, tc.path, hop)
			}
			if !errors.Is(err, ErrNoUpstreamHop) {
				t.Errorf("err = %v, want ErrNoUpstreamHop by identity; callers MUST switch on identity because these messages are written for an operator and will be reworded", err)
			}
			if hop != "" {
				t.Errorf("UpstreamHop returned hop %q alongside an error; a caller that ignored the error would dial it", hop)
			}
			if tc.markerFree {
				if strings.Contains(err.Error(), abpMarker) {
					t.Errorf("the refusal ECHOED the offending hop: %v. A hop on a stored path is a peer's bytes, and the convention here is to report the INDEX and the LENGTH", err)
				}
			} else {
				// The bounded-echo property, which is what actually holds.
				if !strings.Contains(err.Error(), abpMarker) {
					t.Errorf("this case is exempted from the anti-oracle assertion because ids.ValidateBusID echoes what it rejects — but it did not echo. Re-read UpstreamHop step 2 and tighten the assertion above: %v", err)
				}
				if len(err.Error()) > 4*MaxPeerBusIDLen+512 {
					t.Errorf("the refusal is %d bytes long; a remote party must not be able to size this bus's log line: %v", len(err.Error()), err)
				}
			}
		})
	}

	// AND THE HAPPY PATH, so the table above cannot be passing because
	// UpstreamHop refuses everything.
	t.Run("a well-formed stored path resolves the hop before ours", func(t *testing.T) {
		hop, err := UpstreamHop(abpBusB, []string{abpBusA, abpBusB})
		if err != nil {
			t.Fatalf("UpstreamHop: %v", err)
		}
		if hop != abpBusA {
			t.Errorf("hop = %q, want %q", hop, abpBusA)
		}
		// Bus ids are compared case-insensitively, which is what this
		// repository already does everywhere the comparison is load-bearing.
		hop, err = UpstreamHop(strings.ToUpper(abpBusB), []string{abpBusA, abpBusB})
		if err != nil {
			t.Fatalf("UpstreamHop (case-folded local id): %v", err)
		}
		if hop != abpBusA {
			t.Errorf("hop = %q, want %q; ids.BusIDPattern admits both cases, so a case-SENSITIVE test would make a bus fail to recognise its own position on the path", hop, abpBusA)
		}
	})
}

// ---------------------------------------------------------------------------
// DisposeAck
// ---------------------------------------------------------------------------

// TestDisposeAckOriginAndForward pins the three answers DisposeAck can give and,
// most importantly, that the ORIGIN case does not consult the path AT ALL.
func TestDisposeAckOriginAndForward(t *testing.T) {
	key := abpKey(t, 61)

	t.Run("the origin STOPS and never looks at the path", func(t *testing.T) {
		// A DELIBERATELY ABSURD PATH. If the origin test consulted it — say
		// "forward unless the upstream is missing" — the stop condition would
		// depend on a stored field, and a wrong or malicious path would restore
		// the loop §8.4 exists to forbid.
		absurd := [][]string{
			nil,
			{},
			{abpBusD},
			{abpBusD, abpBusC, abpBusD},
			{strings.Repeat("!", 300), "", abpMarker + "."},
		}
		for i, path := range absurd {
			disp, upstream, err := DisposeAck(abpBusA, key, path)
			if err != nil {
				t.Fatalf("path %d %v: DisposeAck at the origin errored: %v", i, path, err)
			}
			if disp != AckStopAtOrigin {
				t.Errorf("path %d %v: disposition = %s, want stop_at_origin", i, path, disp)
			}
			if upstream != "" {
				t.Errorf("path %d %v: upstream = %q, want empty", i, path, upstream)
			}
		}
	})

	t.Run("the origin test folds case", func(t *testing.T) {
		disp, _, err := DisposeAck(strings.ToUpper(abpBusA), key, nil)
		if err != nil {
			t.Fatalf("DisposeAck: %v", err)
		}
		if disp != AckStopAtOrigin {
			t.Errorf("disposition = %s, want stop_at_origin; a single flipped character must not make the origin fail to recognise its own key and forward the settlement back out", disp)
		}
	})

	t.Run("a non-origin key resolves the upstream hop", func(t *testing.T) {
		disp, upstream, err := DisposeAck(abpBusB, key, []string{abpBusA, abpBusB})
		if err != nil {
			t.Fatalf("DisposeAck: %v", err)
		}
		if disp != AckForwardUpstream {
			t.Fatalf("disposition = %s, want forward_upstream", disp)
		}
		if upstream != abpBusA {
			t.Errorf("upstream = %q, want %q", upstream, abpBusA)
		}
	})

	t.Run("an unparseable correlation key is an INVALID FRAME, not a missing hop", func(t *testing.T) {
		// The fault is in the value we were handed, it is decidable by whoever
		// produced it without asking us, and ack.go already owns that meaning.
		for _, bad := range []string{
			"",
			"nodashhere",
			abpBusA + "-",
			abpBusA + "-notanumber",
			abpBusA + "-007",
			strings.Repeat("z", ids.MaxMessageIDLen+10),
		} {
			disp, upstream, err := DisposeAck(abpBusB, bad, []string{abpBusA, abpBusB})
			if !errors.Is(err, ErrInvalidAckFrame) {
				t.Errorf("DisposeAck(%q) err = %v, want ErrInvalidAckFrame", bad, err)
			}
			if errors.Is(err, ErrNoUpstreamHop) {
				t.Errorf("DisposeAck(%q) reported a missing HOP for a malformed KEY; the two have different operators and different remedies", bad)
			}
			if disp != 0 || upstream != "" {
				t.Errorf("DisposeAck(%q) = (%s, %q) alongside an error, want the zero disposition and no hop", bad, disp, upstream)
			}
		}
	})

	t.Run("our own invalid bus id fails CLOSED", func(t *testing.T) {
		// An empty or malformed localBusID compares unequal to the origin half
		// of EVERY correlation key, so a bus with a broken id would classify
		// its own acknowledgements as "forward upstream" and emit the origin's
		// private settlements onto the network.
		for _, bad := range []string{"", "bad.id", strings.Repeat("z", 200)} {
			disp, upstream, err := DisposeAck(bad, key, []string{abpBusA, abpBusB})
			if err == nil {
				t.Errorf("DisposeAck with local bus id %q succeeded as %s/%q", bad, disp, upstream)
			}
			if disp != 0 || upstream != "" {
				t.Errorf("DisposeAck(%q) = (%s, %q) alongside an error", bad, disp, upstream)
			}
		}
	})

	t.Run("the disposition enum is CLOSED and has no zero member", func(t *testing.T) {
		// A zero value is what an unpopulated struct carries. If zero meant
		// "stop at the origin" then every uninitialised disposition would
		// silently swallow a terminal outcome — the least detectable failure
		// this plane could have, because nothing errors and the sender simply
		// never learns.
		if AckDisposition(0).String() != "AckDisposition(0)" {
			t.Errorf("the zero disposition renders as %q; a bogus value must LOOK bogus in a log, not like a decision somebody made", AckDisposition(0))
		}
		var seen int
		for d := AckStopAtOrigin; d < ackDispositionCount; d++ {
			if strings.HasPrefix(d.String(), "AckDisposition(") {
				t.Errorf("disposition %d has no spelling", uint8(d))
			}
			seen++
		}
		if seen != 2 {
			t.Errorf("the enum has %d members, want 2 (stop_at_origin, forward_upstream)", seen)
		}
	})
}

// ---------------------------------------------------------------------------
// AckFrameFrom
// ---------------------------------------------------------------------------

// TestAckFrameFromForwardsVerbatim pins the ONE spelling of "forward verbatim",
// including the two absences that must stay absent and the copy that keeps a
// forwarded frame immune to a caller still holding the source.
func TestAckFrameFromForwardsVerbatim(t *testing.T) {
	key := abpKey(t, 71)
	recipient := abpRecipient(t)
	sig := abpSignature()

	base := func(outcome AckOutcome, class AckClass, signature []byte, attested AckAttestation) ValidatedPeerAck {
		return ValidatedPeerAck{
			ProtocolVersion:    AckWireVersion,
			CorrelationKey:     key,
			Recipient:          recipient,
			Outcome:            outcome,
			Class:              class,
			Attestation:        attested,
			Signature:          signature,
			EmittedAtUnixMilli: 1_755_000_000_123,
		}
	}

	t.Run("a positive terminal carries NO class and DOES carry the attestation", func(t *testing.T) {
		frame := AckFrameFrom(base(AckDelivered, ackNoClass, sig, AckAttestedRecipientSignatureUnverified))
		if frame.Class != "" {
			t.Errorf("class = %q, want empty. A zero enum stringifies to \"AckClass(0)\", a spelling ParseAckClass refuses, so the next hop would answer 400 — which is FINAL, and the outcome would be lost rather than retried", frame.Class)
		}
		if frame.Attestation == nil {
			t.Fatal("a recipient-sourced outcome lost its attestation envelope")
		}
		if !bytes.Equal(frame.Attestation.Signature, sig) {
			t.Errorf("signature = %x, want %x", frame.Attestation.Signature, sig)
		}
		if frame.CorrelationKey != key || frame.Recipient != recipient {
			t.Errorf("ids = (%q,%q), want (%q,%q)", frame.CorrelationKey, frame.Recipient, key, recipient)
		}
		if frame.Outcome != AckDelivered.String() {
			t.Errorf("outcome = %q, want %q", frame.Outcome, AckDelivered.String())
		}
		if frame.EmittedAtUnixMilli != 1_755_000_000_123 {
			t.Errorf("emitted_at = %d; it is the EMITTER's clock and restamping it would make every hop look like the origin of the outcome", frame.EmittedAtUnixMilli)
		}
	})

	t.Run("a negative terminal carries its class", func(t *testing.T) {
		for _, class := range []AckClass{
			AckRecipientRefusedPolicy,
			AckRecipientRefusedUndecodable,
			AckRecipientRefusedNotAddressed,
		} {
			frame := AckFrameFrom(base(AckRefused, class, sig, AckAttestedRecipientSignatureUnverified))
			if frame.Class != class.String() {
				t.Errorf("class = %q, want %q", frame.Class, class.String())
			}
		}
	})

	t.Run("a BUS-sourced outcome carries no attestation at all", func(t *testing.T) {
		// `omitempty` on the POINTER is what makes absent mean absent rather
		// than "present and empty"; ValidateAckAttestation refuses a
		// bus-sourced outcome that carries one.
		frame := AckFrameFrom(base(AckUndeliverable, AckNoRoute, nil, AckAttestedPeerBus))
		if frame.Attestation != nil {
			t.Errorf("attestation = %+v, want nil", frame.Attestation)
		}
		if frame.Class != AckNoRoute.String() {
			t.Errorf("class = %q, want %q", frame.Class, AckNoRoute.String())
		}
		// And an EMPTY (non-nil) signature slice is likewise not an envelope.
		frame = AckFrameFrom(base(AckUndeliverable, AckNoRoute, []byte{}, AckAttestedPeerBus))
		if frame.Attestation != nil {
			t.Errorf("an empty signature produced an attestation envelope %+v; an object with no signature in it is refused by ValidatePeerAckRequest", frame.Attestation)
		}
	})

	t.Run("ProtocolVersion is left ZERO", func(t *testing.T) {
		// Client.PeerAck overwrites it with AckWireVersion by design — "every
		// frame this bus emits declares the version this bus speaks". A second
		// assignment here would be a second place to update at the next bump,
		// and the two would eventually disagree. Do not copy v.ProtocolVersion,
		// which is what the PREVIOUS hop spoke.
		for _, declared := range []int{0, AckWireVersion, 99} {
			v := base(AckDelivered, ackNoClass, sig, AckAttestedRecipientSignatureUnverified)
			v.ProtocolVersion = declared
			if got := AckFrameFrom(v).ProtocolVersion; got != 0 {
				t.Errorf("AckFrameFrom(ProtocolVersion=%d).ProtocolVersion = %d, want 0", declared, got)
			}
		}
	})

	t.Run("the signature is COPIED, never aliased", func(t *testing.T) {
		src := abpSignature()
		v := base(AckDelivered, ackNoClass, src, AckAttestedRecipientSignatureUnverified)
		frame := AckFrameFrom(v)
		if frame.Attestation == nil {
			t.Fatal("no attestation envelope")
		}
		before := append([]byte(nil), frame.Attestation.Signature...)

		// MUTATE THE SOURCE AFTER THE CALL. A forwarded frame must not be
		// mutable through a ValidatedPeerAck the caller still holds — the
		// failure is silent right up to the moment something mutates it.
		for i := range v.Signature {
			v.Signature[i] ^= 0xFF
		}
		if !bytes.Equal(frame.Attestation.Signature, before) {
			t.Fatalf("the forwarded signature changed to %x when the SOURCE was mutated: it is aliased, not copied", frame.Attestation.Signature)
		}
		if bytes.Equal(frame.Attestation.Signature, v.Signature) {
			t.Fatal("the forwarded signature tracks the source slice byte for byte after mutation; it is the same backing array")
		}
	})
}

// ---------------------------------------------------------------------------
// TestAuthorizePeerAckViaIndirectHop — the transit arm of the binding rule
// ---------------------------------------------------------------------------

// abvRoutes is a fake NextHopAddress: PRESENCE in the map is "this bus resolves"
// and the value is the address. A present-but-empty entry is deliberately
// representable even though Registry.PeerBaseURL folds that into "not found" —
// the helper must not rely on the production implementation's politeness, and
// the "both blank" row below is what proves it does not.
type abvRoutes map[string]string

// abvNextHop returns the fake resolver and a pointer to its call count, so the
// direct-arm row can assert the indirect arm was NEVER CONSULTED rather than
// merely that it agreed.
func abvNextHop(routes abvRoutes) (NextHopAddress, *int) {
	calls := 0
	return func(busID string) (string, bool) {
		calls++
		addr, ok := routes[busID]
		return addr, ok
	}, &calls
}

// TestAuthorizePeerAckViaIndirectHop drives AuthorizePeerAckVia's widening of
// ACK-CONTRACT.md §6.2 over an adversarial obligation table.
//
// # THE DEFECT THIS PINS, WHICH EVERY SINGLE-HOP TEST MISSED
//
// In A -> B -> C, A's outbox obligation is DeriveJobID(C, K) — Forwarder.targets
// keys the job on Registry.Route(recipient), the recipient's HOME bus — while the
// acknowledgement arrives over A's authenticated link with B. DeriveJobID(P, K)
// and DeriveJobID(D, K) coincide ONLY on a direct peer link, so the direct rule
// alone refuses every transit acknowledgement, and B's 409-absorbing arm converts
// that refusal into a 200 for the recipient: "accepted" for an outcome nothing
// recorded anywhere (§1.1).
//
// # WHAT EACH ROW IS FOR
//
// The three negative routing rows are the SECURITY CORE, not completeness. The
// widening lets a peer-supplied recipient SELECT which obligation we look for, so
// the only thing standing between that and "any bound peer settles any
// destination" is the clause that the address we would dial for D is the address
// we would dial for P — asked of OUR OWN peer configuration, never of the frame.
// Delete that comparison and the "routes somewhere else" row goes red; it has
// been mutation-proven to do so.
//
// Every refusal here must be ErrAckNotBound BY IDENTITY. The widening adds a way
// to be BOUND and must add no new way to be told WHY you were not, or it becomes
// the existence oracle ErrAckNotBound's doc exists to close.
func TestAuthorizePeerAckViaIndirectHop(t *testing.T) {
	t.Parallel()

	const (
		local = akLocalBus  // A, the origin: it minted the correlation key
		peerP = akPeerBus   // B, the ADJACENT bus whose certificate authenticated
		destD = akThirdBus  // C, the recipient's HOME bus, reached THROUGH B
		other = akOtherPeer // an unrelated peer, at its own address

		viaB = "https://b.example:8443"
		viaX = "https://x.example:8443"
	)

	key := akMessageID(t, local, 11)
	onD := akAgentID(t, destD, "recipient")
	onP := akAgentID(t, peerP, "recipient")
	onLocal := akAgentID(t, local, "recipient")

	directJob := akTable{DeriveJobID(peerP, key): akObligation(peerP, key)}
	indirectJob := akTable{DeriveJobID(destD, key): akObligation(destD, key)}
	localJob := akTable{DeriveJobID(local, key): akObligation(local, key)}
	// Both spellings present: the direct one must win, and it is the record the
	// caller gets back.
	bothJobs := akTable{
		DeriveJobID(peerP, key): akObligation(peerP, key),
		DeriveJobID(destD, key): akObligation(destD, key),
	}

	tests := []struct {
		name string
		jobs akTable
		// routes is the fake routing table; nilNextHop overrides it with nil,
		// which must reproduce the direct arm's answer exactly.
		routes     abvRoutes
		nilNextHop bool
		recipient  string
		// wantBoundTo is the PeerBusID of the record expected back; "" means the
		// call must refuse.
		wantBoundTo string
		wantErr     error
		// wantNoRouting asserts the routing table was never consulted — the
		// direct arm must short-circuit before it.
		wantNoRouting bool
	}{
		{
			// The recipient lives ON the acknowledging peer, so the direct arm's
			// recipient binding (ACK-4-FU-RECIPIENT-BINDING) is satisfied and the
			// arm binds without ever consulting routing.
			name:          "direct job present short-circuits before any routing lookup",
			jobs:          directJob,
			routes:        abvRoutes{destD: viaB, peerP: viaB},
			recipient:     onP,
			wantBoundTo:   peerP,
			wantNoRouting: true,
		},
		{
			// Recipient on the peer: the direct arm binds peerP and short-circuits
			// even though an indirect job for destD also sits in the table.
			name:          "direct job wins even when an indirect one also exists",
			jobs:          bothJobs,
			routes:        abvRoutes{destD: viaB, peerP: viaB},
			recipient:     onP,
			wantBoundTo:   peerP,
			wantNoRouting: true,
		},
		{
			// ACK-4-FU-RECIPIENT-BINDING at the Via level: a direct job for the
			// peer does NOT let it settle a recipient whose home bus is a DIFFERENT
			// bus. The direct arm refuses (home bus destD != peer), and the indirect
			// arm cannot rescue it because destD routes to a different address than
			// the acking peer. Before the binding this bound to peerP — the forgery.
			name:      "direct job does NOT let the peer settle a recipient on another bus",
			jobs:      directJob,
			routes:    abvRoutes{destD: viaX, peerP: viaB},
			recipient: onD,
			wantErr:   ErrAckNotBound,
		},
		{
			name:        "indirect: job for the destination and one shared next hop",
			jobs:        indirectJob,
			routes:      abvRoutes{destD: viaB, peerP: viaB},
			recipient:   onD,
			wantBoundTo: destD,
		},
		{
			name:      "indirect refused when the destination routes somewhere else",
			jobs:      indirectJob,
			routes:    abvRoutes{destD: viaX, peerP: viaB},
			recipient: onD,
			wantErr:   ErrAckNotBound,
		},
		{
			name:      "indirect refused when the destination does not resolve",
			jobs:      indirectJob,
			routes:    abvRoutes{peerP: viaB},
			recipient: onD,
			wantErr:   ErrAckNotBound,
		},
		{
			name:      "indirect refused when the acknowledging peer does not resolve",
			jobs:      indirectJob,
			routes:    abvRoutes{destD: viaB},
			recipient: onD,
			wantErr:   ErrAckNotBound,
		},
		{
			// Two buses with no address configured must not become each other's
			// next hop by both being blank.
			name:      "indirect refused when both resolve to the empty address",
			jobs:      indirectJob,
			routes:    abvRoutes{destD: "", peerP: ""},
			recipient: onD,
			wantErr:   ErrAckNotBound,
		},
		{
			name:      "indirect refused when no obligation names the destination",
			jobs:      akTable{},
			routes:    abvRoutes{destD: viaB, peerP: viaB},
			recipient: onD,
			wantErr:   ErrAckNotBound,
		},
		{
			// The recipient is at home HERE, so nothing was ever routed for it.
			// An obligation to OURSELVES cannot exist in a real outbox; the
			// adversarial table holds one anyway, and it must still refuse.
			name:      "indirect refused for a recipient on this bus",
			jobs:      localJob,
			routes:    abvRoutes{local: viaB, peerP: viaB},
			recipient: onLocal,
			wantErr:   ErrAckNotBound,
		},
		{
			// D == P is the direct arm's own question; re-asking it must not
			// produce a second answer.
			name:      "recipient at home on the acknowledging peer falls to the direct arm",
			jobs:      indirectJob,
			routes:    abvRoutes{peerP: viaB, destD: viaB},
			recipient: onP,
			wantErr:   ErrAckNotBound,
		},
		{
			name:      "a malformed recipient is an invalid frame, not a refusal",
			jobs:      indirectJob,
			routes:    abvRoutes{destD: viaB, peerP: viaB},
			recipient: "not-fully-qualified",
			wantErr:   ErrInvalidAckFrame,
		},
		{
			// Recipient on the peer, so the direct arm binds; the nil routing table
			// must reproduce that exact answer.
			name:        "nil routing table reproduces the direct arm: bound",
			jobs:        directJob,
			nilNextHop:  true,
			recipient:   onP,
			wantBoundTo: peerP,
		},
		{
			name:       "nil routing table reproduces the direct arm: refused",
			jobs:       indirectJob,
			nilNextHop: true,
			recipient:  onD,
			wantErr:    ErrAckNotBound,
		},
		{
			name:      "an unrelated peer at its own address binds nothing",
			jobs:      akTable{DeriveJobID(other, key): akObligation(other, key)},
			routes:    abvRoutes{destD: viaB, peerP: viaB, other: viaX},
			recipient: onD,
			wantErr:   ErrAckNotBound,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				nextHop NextHopAddress
				calls   *int
			)
			if !tc.nilNextHop {
				nextHop, calls = abvNextHop(tc.routes)
			}

			rec, err := AuthorizePeerAckVia(tc.jobs, nextHop, local, peerP, key, tc.recipient)

			// The direct arm's answer, computed independently, is what the nil
			// routing table must reproduce exactly.
			if tc.nilNextHop {
				wantRec, wantErr := AuthorizePeerAck(tc.jobs, local, peerP, key, tc.recipient)
				if rec != wantRec || !errorsEqualAbv(err, wantErr) {
					t.Fatalf("nil NextHopAddress: got (%+v, %v), want the direct arm's exact answer (%+v, %v)", rec, err, wantRec, wantErr)
				}
			}

			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				if rec != (OutboxRecord{}) {
					t.Fatalf("a refusal returned a record: %+v", rec)
				}
				// THE UNIFORM ANSWER. A refusal on this arm must be the SAME
				// sentinel, with the SAME text, as the direct arm's — a new
				// spelling would be a new distinguishable case, which is the
				// oracle ErrAckNotBound is written to prevent.
				if tc.wantErr == ErrAckNotBound && err.Error() != ErrAckNotBound.Error() {
					t.Fatalf("refusal text %q is not byte-identical to the uniform refusal %q", err.Error(), ErrAckNotBound.Error())
				}
			default:
				if err != nil {
					t.Fatalf("got err %v, want the frame bound", err)
				}
				if rec.PeerBusID != tc.wantBoundTo {
					t.Fatalf("bound to peer %q, want %q", rec.PeerBusID, tc.wantBoundTo)
				}
				if rec.OriginMessageID != key {
					t.Fatalf("bound record carries origin message id %q, want %q", rec.OriginMessageID, key)
				}
				if rec.JobID != DeriveJobID(tc.wantBoundTo, key) {
					t.Fatalf("bound record job id %q, want %q", rec.JobID, DeriveJobID(tc.wantBoundTo, key))
				}
			}

			if tc.wantNoRouting && calls != nil && *calls != 0 {
				t.Fatalf("the routing table was consulted %d times; the direct arm must short-circuit before it", *calls)
			}
		})
	}
}

// errorsEqualAbv compares two errors by text, which is what "the same answer"
// means for this package's sentinels: they are returned by identity
// (ErrAckNotBound) or wrapped with a formatted detail (ErrInvalidAckFrame), and
// both must match exactly for the nil-routing-table rows.
func errorsEqualAbv(got, want error) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return got.Error() == want.Error()
}
