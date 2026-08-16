package relay

// ACK-3 — the peer-hop ACK/NACK wire semantics and correlation.
//
// The two tests the stored proof command names are TestRelayHopAckNackAuthentication
// (who may settle what, and what a refusal costs) and TestRelayHopAckNackCorrelation
// (which durable obligation a frame is bound to, and how a version and a
// duplicate are read). Everything else in this file supports one of them.
//
// NO TEST HERE NAMES A PEER ROUTE PATH OUTSIDE THIS PACKAGE'S OWN CONSTANTS —
// guards_test.go permits the constant inside internal/relay and forbids it
// almost everywhere else.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	ackLocalBus  = "acklocal"
	ackPeerBus   = "ackpeer"
	ackOtherPeer = "ackother"
	ackOriginBus = "ackorigin"
)

// ackTable is an adversarial AckObligations: a plain map of job id to record,
// with no sweeping and no locking beyond a mutex, so a test can put an
// obligation table into any shape including ones *Outbox would never produce.
//
// AuthorizePeerAck is deliberately written against the INTERFACE for exactly
// this, and the production implementation is still the outbox and only the
// outbox — TestRelayHopAckNackCorrelation drives a REAL *Outbox for the same
// assertions, so nothing here rests on the stub agreeing with it.
type ackTable struct {
	mu      sync.Mutex
	jobs    map[string]OutboxRecord
	lookups atomic.Int64
}

func newAckTable() *ackTable { return &ackTable{jobs: map[string]OutboxRecord{}} }

func (t *ackTable) Lookup(jobID string) (OutboxRecord, bool) {
	t.lookups.Add(1)
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.jobs[jobID]
	return r, ok
}

// owe records that this bus owes peerBusID a copy of correlationKey, exactly as
// Forwarder.Enqueue would.
func (t *ackTable) owe(peerBusID, correlationKey string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.jobs[DeriveJobID(peerBusID, correlationKey)] = OutboxRecord{
		JobID:           DeriveJobID(peerBusID, correlationKey),
		PeerBusID:       peerBusID,
		OriginMessageID: correlationKey,
		State:           OutboxPending,
	}
}

// ackRecorder is a SettleAck callback that records what it was handed.
type ackRecorder struct {
	mu        sync.Mutex
	calls     []SettledAck
	duplicate bool
	err       error
}

func (r *ackRecorder) settle(_ context.Context, s SettledAck) (AckSettlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
	if r.err != nil {
		return AckSettlement{}, r.err
	}
	return AckSettlement{Duplicate: r.duplicate}, nil
}

func (r *ackRecorder) last() (SettledAck, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return SettledAck{}, false
	}
	return r.calls[len(r.calls)-1], true
}

func (r *ackRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// ackSink captures the operator log so a refusal can be asserted LOUD as well
// as uniform (invariant 6: the silent discard is the defect).
type ackSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *ackSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *ackSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// ackRig is one handler with everything a test needs to drive and inspect it.
type ackRig struct {
	h       *AckHandler
	table   *ackTable
	settled *ackRecorder
	logs    *ackSink

	// admitted and admitErr drive the meter. admitErr is set to refuse.
	admitted atomic.Int64
	released atomic.Int64
	admitErr error

	// lookupAtAdmit is how many obligation lookups had ALREADY happened at the
	// instant the meter ran, so the ordering assertion is about observed
	// sequence rather than about reading the source. lastAdmitPeer is the id the
	// meter was keyed on.
	lookupAtAdmit atomic.Int64
	lastAdmitPeer atomic.Value
}

func newAckRig(t *testing.T, tune func(*AckConfig)) *ackRig {
	t.Helper()
	rig := &ackRig{table: newAckTable(), settled: &ackRecorder{}, logs: &ackSink{}}
	cfg := AckConfig{
		BusID:       ackLocalBus,
		Obligations: rig.table,
		Admit: func(peerBusID string) (func(), error) {
			rig.lastAdmitPeer.Store(peerBusID)
			// The number of obligation lookups AT THE MOMENT the meter ran. If it
			// is not zero, the meter is behind the expensive call.
			rig.lookupAtAdmit.Store(rig.table.lookups.Load())
			if rig.admitErr != nil {
				return nil, rig.admitErr
			}
			rig.admitted.Add(1)
			return func() { rig.released.Add(1) }, nil
		},
		SettleAck: rig.settled.settle,
		Logger:    logging.New(rig.logs, logging.LevelDebug),
	}
	if tune != nil {
		tune(&cfg)
	}
	h, err := NewAckHandler(cfg)
	if err != nil {
		t.Fatalf("NewAckHandler: %v", err)
	}
	rig.h = h
	return rig
}

// post drives ServeAuthenticated with peerBusID as the AUTHENTICATED principal.
func (rig *ackRig) post(t *testing.T, peerBusID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	switch v := body.(type) {
	case string:
		buf = []byte(v)
	default:
		var err error
		if buf, err = json.Marshal(v); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, PeerAckPath, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rig.h.ServeAuthenticated(rec, req, peerBusID)
	return rec
}

// ackMessageID is a well-formed origin message id through the real minter.
func ackMessageID(t testing.TB, seq uint64) string {
	t.Helper()
	id, err := ids.MessageID(ackOriginBus, seq)
	if err != nil {
		t.Fatalf("ids.MessageID: %v", err)
	}
	return id
}

// ackAgentID is a well-formed fully-qualified agent id (invariant 2).
func ackAgentID(t testing.TB, bus, name string) string {
	t.Helper()
	id, err := ids.AgentID(bus, name, 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	return id
}

// ackFrame is a minimal VALID recipient-sourced ACK for key and recipient.
func ackFrame(key, recipient string) PeerAckRequest {
	return PeerAckRequest{
		ProtocolVersion:    AckWireVersion,
		CorrelationKey:     key,
		Recipient:          recipient,
		Outcome:            AckDelivered.String(),
		EmittedAtUnixMilli: 1_700_000_000_000,
		Attestation:        &AckAttestationEnvelope{Signature: make([]byte, signing.SignatureSize)},
	}
}

// errorCodeOf reads the stable code out of a refusal body.
func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding error body %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

// ---------------------------------------------------------------------------
// 1. Authentication, authorization and the cost of a refusal
// ---------------------------------------------------------------------------

// TestRelayHopAckNackAuthentication is half of this task's proof command.
//
// It asserts the properties that decide WHO MAY SETTLE WHAT, and it is built
// around the one that is invisible if you only test the happy path: the peer bus
// id AuthorizePeerAck binds on comes from the AUTHENTICATED PRINCIPAL and from
// nowhere a peer can influence.
func TestRelayHopAckNackAuthentication(t *testing.T) {
	key := ackMessageID(t, 7)
	recipient := ackAgentID(t, ackLocalBus, "bravo")

	// -----------------------------------------------------------------------
	t.Run("the frame carries NO field a peer bus id could be read from", func(t *testing.T) {
		// STRUCTURAL, not behavioural. AuthorizePeerAck authorises
		// DeriveJobID(peerBusID, key); if the frame could name a bus, a peer would
		// be authorising its own choice of victim.
		//
		// IT IS ASSERTED OVER THE STRUCT TYPE BY REFLECTION, NOT OVER A
		// MARSHALLED INSTANCE, AND THAT DISTINCTION IS THE WHOLE TEST. An earlier
		// version of this marshalled a fixture and read the keys back — and a
		// mutation that added `PeerBus string \`json:"peer_bus,omitempty"\`` and
		// USED IT slipped straight past, because omitempty leaves an unset field
		// out of the JSON entirely. The fixture never reached the boundary it was
		// meant to guard. Reading the tags off the type sees the field whether or
		// not any instance populates it.
		typ := reflect.TypeOf(PeerAckRequest{})
		want := map[string]bool{
			"protocol_version": true, "correlation_key": true, "recipient": true,
			"outcome": true, "class": true, "emitted_at": true, "attestation": true,
		}
		seen := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			seen[name] = true
			if !want[name] {
				t.Errorf("PeerAckRequest declares an unexpected field %q (Go field %s). If it names a bus in ANY spelling it must be REMOVED: the binding rule would then authorise the name a peer chose rather than the peer that sent it (ackframe.go's header)",
					name, typ.Field(i).Name)
			}
		}
		for k := range want {
			if !seen[k] {
				t.Errorf("PeerAckRequest no longer declares %q; the frame's shape is part of the protocol", k)
			}
		}
		// The same rule stated the other way round, so a field named something
		// nobody thought to enumerate above is still caught by intent.
		for i := 0; i < typ.NumField(); i++ {
			lower := strings.ToLower(typ.Field(i).Name + " " + typ.Field(i).Tag.Get("json"))
			if strings.Contains(lower, "bus") {
				t.Errorf("PeerAckRequest field %s mentions a bus. The authenticated peer id is a Go PARAMETER supplied by the mount from the TLS client certificate, and there must be nothing in the frame to read it from instead",
					typ.Field(i).Name)
			}
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a peer that invents a bus-id field is REFUSED, not silently ignored", func(t *testing.T) {
		// decodeStrict's DisallowUnknownFields is what makes the absence above a
		// rule rather than a convention: a peer cannot smuggle one in and hope a
		// future handler reads it.
		rig := newAckRig(t, nil)
		rig.table.owe(ackPeerBus, key)
		body := fmt.Sprintf(`{"protocol_version":1,"correlation_key":%q,"recipient":%q,"outcome":"undeliverable","class":"no_route","emitted_at":1,"peer_bus":%q}`,
			key, recipient, ackOtherPeer)
		rec := rig.post(t, ackPeerBus, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want %d — an unknown field must be refused", rec.Code, http.StatusBadRequest)
		}
		if rig.settled.count() != 0 {
			t.Error("a frame with an unknown field reached the durable settle")
		}
	})

	// -----------------------------------------------------------------------
	t.Run("THE FORGERY: peer B cannot settle the obligation this bus owes peer C", func(t *testing.T) {
		// THIS IS THE TEST THE peerBusID-FROM-PRINCIPAL RULE EXISTS FOR, and it
		// is the one that goes RED if the handler ever reads the bus id from
		// anything the attacker controls.
		//
		// The setup is the cross-route forgery §6.2 names: this bus owes the
		// message to ackPeerBus and to NOBODY else. ackOtherPeer, a legitimately
		// peered and legitimately authenticated bus, tries to settle it.
		rig := newAckRig(t, nil)
		rig.table.owe(ackPeerBus, key)

		rec := rig.post(t, ackOtherPeer, ackFrame(key, recipient))
		if rec.Code != http.StatusConflict {
			t.Fatalf("a peer settling ANOTHER peer's obligation got status %d, want %d", rec.Code, http.StatusConflict)
		}
		if rig.settled.count() != 0 {
			t.Fatal("a cross-route forgery reached the durable settle; the binding rule did not run on the AUTHENTICATED peer id")
		}

		// AND THE HONEST PEER SUCCEEDS with a byte-identical frame. This pairing
		// is what proves the refusal came from the PRINCIPAL and not from
		// something in the bytes: identical body, different authenticated peer,
		// opposite outcome.
		ok := newAckRig(t, nil)
		ok.table.owe(ackPeerBus, key)
		if rec := ok.post(t, ackPeerBus, ackFrame(key, recipient)); rec.Code != http.StatusOK {
			t.Fatalf("the peer this bus actually owes got status %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
	})

	// -----------------------------------------------------------------------
	t.Run("reflection: a peer settling a key we never owed anyone is refused", func(t *testing.T) {
		rig := newAckRig(t, nil)
		rec := rig.post(t, ackPeerBus, ackFrame(key, recipient))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusConflict)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("every uniform refusal is BYTE-IDENTICAL on the wire", func(t *testing.T) {
		// THE UNIFORMITY IS THE SECURITY PROPERTY. A distinguishable answer would
		// let any peered bus probe "did bus A send message K to bus B", and by
		// extension whether a named agent exists and is being written to.
		bodies := map[string]string{}
		for _, tc := range []struct {
			name  string
			setup func(*ackRig)
			peer  string
		}{
			{"never owed to anyone", func(*ackRig) {}, ackPeerBus},
			{"owed to a different peer", func(r *ackRig) { r.table.owe(ackOtherPeer, key) }, ackPeerBus},
			{"owed for a different key", func(r *ackRig) { r.table.owe(ackPeerBus, ackMessageID(t, 99)) }, ackPeerBus},
			{"no ack row for the pair", func(r *ackRig) {
				r.table.owe(ackPeerBus, key)
				r.settled.err = ErrAckNotBound
			}, ackPeerBus},
		} {
			rig := newAckRig(t, nil)
			tc.setup(rig)
			rec := rig.post(t, tc.peer, ackFrame(key, recipient))
			if rec.Code != http.StatusConflict {
				t.Fatalf("%s: status %d, want %d", tc.name, rec.Code, http.StatusConflict)
			}
			bodies[tc.name] = rec.Body.String()
		}
		var first, firstName string
		for name, body := range bodies {
			if firstName == "" {
				first, firstName = body, name
				continue
			}
			if body != first {
				t.Errorf("refusal bodies differ: %s = %s, %s = %s. Every well-formed acknowledgement this bus will not settle must be answered IDENTICALLY, or the difference is an oracle",
					firstName, first, name, body)
			}
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a refusal is LOUD in the log and the ids are ELIDED", func(t *testing.T) {
		// Invariant 6 makes the SILENT discard the defect, so uniform-on-the-wire
		// must not mean invisible-to-the-operator. And the ids are a remote
		// party's bytes, so the operator log must not let a peer choose the size
		// of our log line.
		rig := newAckRig(t, nil)
		huge := strings.Repeat("Z", 4000)
		rec := rig.post(t, ackPeerBus, PeerAckRequest{
			ProtocolVersion:    AckWireVersion,
			CorrelationKey:     huge[:200],
			Recipient:          recipient,
			Outcome:            AckUndeliverable.String(),
			Class:              AckNoRoute.String(),
			EmittedAtUnixMilli: 1,
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("an oversized correlation key got status %d, want %d", rec.Code, http.StatusBadRequest)
		}
		logs := rig.logs.String()
		if !strings.Contains(logs, "REFUSED") && !strings.Contains(logs, "rejected") {
			t.Errorf("the refusal was not logged loudly; log was:\n%s", logs)
		}
		if strings.Contains(logs, huge[:200]) {
			t.Error("the operator log echoed the full oversized correlation key; it must be elided")
		}
	})

	// -----------------------------------------------------------------------
	t.Run("NO refusal on this route closes a connection", func(t *testing.T) {
		// §12: invariant 10's two questions were answered on the record and both
		// point the same way. A peer link carries EVERY AGENT BEHIND IT.
		//
		// Asserted over a REAL server so "did the connection survive" is a fact
		// about a socket rather than about a recorder: every refusal is followed
		// by a legitimate request ON THE SAME CONNECTION, which cannot succeed if
		// anything hung up.
		rig := newAckRig(t, nil)
		rig.table.owe(ackPeerBus, key)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rig.h.ServeAuthenticated(w, r, ackPeerBus)
		}))
		defer srv.Close()

		// ONE CONNECTION, AND REUSE IS ASSERTED RATHER THAN ASSUMED. "The next
		// request succeeded" is NOT evidence that nothing hung up: Go's transport
		// transparently redials, so a handler that closed the connection after
		// every refusal would pass a status-only test perfectly. httptrace's
		// GotConn.Reused is the only thing that can tell the two apart, and a
		// reviewer caught this test claiming a mechanism it did not implement.
		client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1}}
		var reused, total int
		send := func(body interface{}) (int, error) {
			var buf []byte
			switch v := body.(type) {
			case string:
				buf = []byte(v)
			default:
				buf, _ = json.Marshal(v)
			}
			req, err := http.NewRequest(http.MethodPost, srv.URL+PeerAckPath, bytes.NewReader(buf))
			if err != nil {
				return 0, err
			}
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) {
					total++
					if info.Reused {
						reused++
					}
				},
			}))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				return 0, err
			}
			defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
			return resp.StatusCode, nil
		}
		// Every request after the very first must land on the SAME connection.
		defer func() {
			if total < 2 {
				t.Fatalf("only %d connections were observed; the reuse assertion below would be vacuous", total)
			}
			if reused != total-1 {
				t.Errorf("%d of %d requests reused the connection, want %d. A refusal DROPPED the socket — and a peer link carries every agent behind that peer, including their parked long-polls (ACK-CONTRACT.md §12, invariant 10)",
					reused, total, total-1)
			}
		}()

		for _, tc := range []struct {
			name string
			body interface{}
			want int
		}{
			{"malformed JSON", `{`, http.StatusBadRequest},
			{"unknown field", `{"nope":1}`, http.StatusBadRequest},
			{"unknown outcome", PeerAckRequest{ProtocolVersion: 1, CorrelationKey: key, Recipient: recipient, Outcome: "vanished", EmittedAtUnixMilli: 1}, http.StatusBadRequest},
			{"unknown version", PeerAckRequest{ProtocolVersion: 99, CorrelationKey: key, Recipient: recipient, Outcome: "delivered", EmittedAtUnixMilli: 1}, http.StatusBadRequest},
			{"not bound", ackFrame(ackMessageID(t, 4242), recipient), http.StatusConflict},
			{"a peer claiming our own bus id is a 400", func() PeerAckRequest {
				f := ackFrame(key, recipient)
				f.Recipient = "not-fully-qualified"
				return f
			}(), http.StatusBadRequest},
			{"an unsupported media type", "{}", http.StatusBadRequest},
		} {
			got, err := send(tc.body)
			if err != nil {
				t.Fatalf("%s: the request failed at the transport: %v — a refusal must not drop the connection", tc.name, err)
			}
			if got != tc.want {
				t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
			}
			// THE ASSERTION THAT MATTERS: a legitimate frame still works, on the
			// same client, immediately after.
			ok, err := send(ackFrame(key, recipient))
			if err != nil {
				t.Fatalf("after %s the connection was unusable: %v. NO ACK-plane refusal may disconnect (§12)", tc.name, err)
			}
			if ok != http.StatusOK {
				t.Errorf("after %s a legitimate acknowledgement got %d, want 200", tc.name, ok)
			}
		}
	})

	// -----------------------------------------------------------------------
	t.Run("an agent-surface outcome is never labelled as a bus attestation", func(t *testing.T) {
		// The route passes AckSurfacePeer as a literal, so a peer-hop frame is
		// labelled peer_bus for a routing outcome and
		// recipient_signature_unverified for a recipient one — never the reverse,
		// and never "verified", which does not exist.
		rig := newAckRig(t, nil)
		rig.table.owe(ackPeerBus, key)

		frame := ackFrame(key, recipient)
		frame.Outcome = AckUndeliverable.String()
		frame.Class = AckNoRoute.String()
		frame.Attestation = nil
		if rec := rig.post(t, ackPeerBus, frame); rec.Code != http.StatusOK {
			t.Fatalf("a routing outcome got %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		got, _ := rig.settled.last()
		if got.Ack.Attestation != AckAttestedPeerBus {
			t.Errorf("a bus-sourced outcome was labelled %s, want peer_bus", got.Ack.Attestation)
		}

		rig2 := newAckRig(t, nil)
		rig2.table.owe(ackPeerBus, key)
		if rec := rig2.post(t, ackPeerBus, ackFrame(key, recipient)); rec.Code != http.StatusOK {
			t.Fatalf("a recipient outcome got %d, want 200", rec.Code)
		}
		got2, _ := rig2.settled.last()
		if got2.Ack.Attestation != AckAttestedRecipientSignatureUnverified {
			t.Errorf("a recipient-sourced outcome was labelled %s, want recipient_signature_unverified", got2.Ack.Attestation)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("the meter runs BEFORE the obligation lookup and is keyed on the authenticated peer", func(t *testing.T) {
		// Outbox.Lookup takes the outbox's EXCLUSIVE mutex and sweeps O(n)
		// records, so an unmetered path in front of it is a denial of service
		// against every writer on this bus. The ordering is asserted from
		// OBSERVED SEQUENCE — the lookup count at the instant the meter ran — not
		// from reading the source.
		rig := newAckRig(t, nil)
		rig.table.owe(ackPeerBus, key)
		if rec := rig.post(t, ackPeerBus, ackFrame(key, recipient)); rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", rec.Code)
		}
		if got := rig.lookupAtAdmit.Load(); got != 0 {
			t.Errorf("%d obligation lookups had already happened when the meter ran; the meter must be IN FRONT of the O(n) exclusive sweep", got)
		}
		if got := rig.lastAdmitPeer.Load(); got != ackPeerBus {
			t.Errorf("the meter was keyed on %v, want the AUTHENTICATED peer %s — metering on anything a peer chooses lets it pick its own bucket", got, ackPeerBus)
		}
		if rig.admitted.Load() != rig.released.Load() {
			t.Errorf("admitted %d and released %d; every admitted request must release its slot or the bound leaks shut",
				rig.admitted.Load(), rig.released.Load())
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a metered refusal is 503 RETRIABLE and costs nothing durable", func(t *testing.T) {
		// THE STATUS IS LOAD-BEARING. A 4xx is FINAL to
		// PeerRefusedError.Retriable, so a throttled acknowledgement answered 4xx
		// would be ABANDONED by the sender and the recipient's decision would
		// never reach the origin. Nothing was written, so retrying is correct.
		rig := newAckRig(t, nil)
		rig.table.owe(ackPeerBus, key)
		rig.admitErr = errors.New("this peer is at its in-flight limit")
		rec := rig.post(t, ackPeerBus, ackFrame(key, recipient))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("a metered refusal got %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
		if !(&PeerRefusedError{StatusCode: rec.Code}).Retriable() {
			t.Errorf("status %d is not retriable; a throttled acknowledgement would be lost rather than re-offered", rec.Code)
		}
		if rig.settled.count() != 0 {
			t.Error("a metered refusal reached the durable settle")
		}
		if got := rig.table.lookups.Load(); got != 0 {
			t.Errorf("a metered request still cost %d obligation lookups; the whole point of the meter is that it does not", got)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a mount with no principal is refused rather than treated as a peer", func(t *testing.T) {
		// Unreachable behind RequirePeerPrincipal. Checked because the failure is
		// silent: an empty peer id derives a job id nobody owns, so every
		// legitimate acknowledgement would be refused with the uniform answer and
		// the surface would look exactly like a working anti-forgery rule.
		rig := newAckRig(t, nil)
		rig.table.owe("", key)
		rec := rig.post(t, "", ackFrame(key, recipient))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusInternalServerError)
		}
		if rig.settled.count() != 0 {
			t.Error("an unauthenticated request reached the durable settle")
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a peer may not claim OUR bus id", func(t *testing.T) {
		rig := newAckRig(t, nil)
		rec := rig.post(t, ackLocalBus, ackFrame(key, recipient))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("a peer claiming our own bus id got %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("the class half-set is enforced in BOTH directions", func(t *testing.T) {
		// Without it a peer sends outcome=refused with class=no_route and this
		// bus records its OWN routing failure as THE RECIPIENT'S DECISION — or
		// the reverse, attributing a recipient's policy refusal to the federation.
		rig := newAckRig(t, nil)
		rig.table.owe(ackPeerBus, key)
		for _, tc := range []struct {
			name    string
			outcome string
			class   string
			sig     bool
		}{
			{"refused with a BUS class", AckRefused.String(), AckNoRoute.String(), true},
			{"undeliverable with a RECIPIENT class", AckUndeliverable.String(), AckRecipientRefusedPolicy.String(), false},
			{"delivered with a class at all", AckDelivered.String(), AckNoRoute.String(), true},
			{"refused with NO class", AckRefused.String(), "", true},
			{"undeliverable with NO class", AckUndeliverable.String(), "", false},
			{"a recipient outcome with NO attestation", AckDelivered.String(), "", false},
			{"a bus outcome WITH an attestation", AckUndeliverable.String(), AckNoRoute.String(), true},
		} {
			frame := ackFrame(key, recipient)
			frame.Outcome, frame.Class = tc.outcome, tc.class
			if !tc.sig {
				frame.Attestation = nil
			}
			if rec := rig.post(t, ackPeerBus, frame); rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status %d, want %d", tc.name, rec.Code, http.StatusBadRequest)
			}
		}
		if rig.settled.count() != 0 {
			t.Errorf("%d ill-formed frames reached the durable settle", rig.settled.count())
		}
	})

	// -----------------------------------------------------------------------
	t.Run("an attestation object with an empty signature is refused, not treated as absent", func(t *testing.T) {
		rig := newAckRig(t, nil)
		rig.table.owe(ackPeerBus, key)
		frame := ackFrame(key, recipient)
		frame.Outcome, frame.Class = AckUndeliverable.String(), AckNoRoute.String()
		frame.Attestation = &AckAttestationEnvelope{}
		if rec := rig.post(t, ackPeerBus, frame); rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("the transport answers match the other peer surfaces", func(t *testing.T) {
		rig := newAckRig(t, func(c *AckConfig) { c.MaxRequestBytes = 64 })
		rig.table.owe(ackPeerBus, key)

		get := httptest.NewRequest(http.MethodGet, PeerAckPath, nil)
		rec := httptest.NewRecorder()
		rig.h.ServeAuthenticated(rec, get, ackPeerBus)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}

		form := httptest.NewRequest(http.MethodPost, PeerAckPath, strings.NewReader("{}"))
		form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec = httptest.NewRecorder()
		rig.h.ServeAuthenticated(rec, form, ackPeerBus)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("form POST status %d, want %d", rec.Code, http.StatusUnsupportedMediaType)
		}

		if got := rig.post(t, ackPeerBus, ackFrame(key, recipient)); got.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("over-cap POST status %d, want %d", got.Code, http.StatusRequestEntityTooLarge)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("the handler refuses to construct without its required parts", func(t *testing.T) {
		full := func() AckConfig {
			return AckConfig{
				BusID:       ackLocalBus,
				Obligations: newAckTable(),
				Admit:       func(string) (func(), error) { return func() {}, nil },
				SettleAck: func(context.Context, SettledAck) (AckSettlement, error) {
					return AckSettlement{}, nil
				},
			}
		}
		for _, tc := range []struct {
			name   string
			break_ func(*AckConfig)
		}{
			{"no bus id", func(c *AckConfig) { c.BusID = "" }},
			{"no obligation table", func(c *AckConfig) { c.Obligations = nil }},
			{"no meter", func(c *AckConfig) { c.Admit = nil }},
			{"no settle callback", func(c *AckConfig) { c.SettleAck = nil }},
			{"negative byte cap", func(c *AckConfig) { c.MaxRequestBytes = -1 }},
		} {
			cfg := full()
			tc.break_(&cfg)
			if _, err := NewAckHandler(cfg); err == nil {
				t.Errorf("%s: NewAckHandler succeeded; a missing part must fail at startup where an operator can read which side is broken", tc.name)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// 2. Correlation to the durable record
// ---------------------------------------------------------------------------

// TestRelayHopAckNackCorrelation is the other half of this task's proof command.
//
// It asserts that a frame is bound to THE DURABLE OBLIGATION THIS BUS WROTE, on
// the correlation key defined in ACK-CONTRACT.md §3 — the ORIGIN bus's
// server-minted message id, which is already OutboxRecord.OriginMessageID and
// already the relay idempotency key. It runs against a REAL *Outbox over a real
// WAL, so the correlation is proved end to end rather than against a stub that
// might agree with the handler by construction.
func TestRelayHopAckNackCorrelation(t *testing.T) {
	recipient := ackAgentID(t, ackLocalBus, "bravo")

	// newRealOutbox builds an outbox over a real durable log and enqueues one
	// obligation, exactly as Forwarder.Enqueue does on the egress path.
	newRealOutbox := func(t *testing.T, peer, key string) *Outbox {
		t.Helper()
		ob, _, _, _ := obNewOutbox(t, func(o *OutboxOptions) { o.BusID = ackLocalBus })
		if _, err := ob.Enqueue(OutboxJob{
			PeerBusID:       peer,
			OriginMessageID: key,
			Size:            11,
			ContentSHA256:   obHash,
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		return ob
	}

	// -----------------------------------------------------------------------
	t.Run("the correlation key IS the outbox record's OriginMessageID", func(t *testing.T) {
		key := ackMessageID(t, 11)
		ob := newRealOutbox(t, ackPeerBus, key)
		rig := newAckRig(t, func(c *AckConfig) { c.Obligations = ob })

		if rec := rig.post(t, ackPeerBus, ackFrame(key, recipient)); rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		got, ok := rig.settled.last()
		if !ok {
			t.Fatal("nothing reached the durable settle")
		}
		// The obligation handed on is the one THIS BUS wrote, and its
		// OriginMessageID is byte-identical to the correlation key on the wire.
		// That identity is the whole of §3: no fourth identifier, no mapping
		// table, no translation step where the two could drift.
		if got.Obligation.OriginMessageID != key {
			t.Errorf("bound obligation names %q, want the correlation key %q", got.Obligation.OriginMessageID, key)
		}
		if got.Obligation.JobID != DeriveJobID(ackPeerBus, key) {
			t.Errorf("bound job id %q, want DeriveJobID(peer, key) = %q", got.Obligation.JobID, DeriveJobID(ackPeerBus, key))
		}
		if got.PeerBusID != ackPeerBus {
			t.Errorf("settled on behalf of %q, want the authenticated peer %q", got.PeerBusID, ackPeerBus)
		}
		if got.Ack.CorrelationKey != key || got.Ack.Recipient != recipient {
			t.Errorf("settled (%q,%q), want (%q,%q)", got.Ack.CorrelationKey, got.Ack.Recipient, key, recipient)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a real outbox refuses a key it holds for a DIFFERENT peer", func(t *testing.T) {
		key := ackMessageID(t, 12)
		ob := newRealOutbox(t, ackOtherPeer, key)
		rig := newAckRig(t, func(c *AckConfig) { c.Obligations = ob })
		if rec := rig.post(t, ackPeerBus, ackFrame(key, recipient)); rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want %d — the job id is keyed on the PEER", rec.Code, http.StatusConflict)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("multi-hop is NOT broken: the key's bus half need not be the acking peer", func(t *testing.T) {
		// In an A->B->C chain, C's acknowledgement reaches A via B and the bus
		// half of K is A's, not C's. A "the bus half must equal the peer" rule
		// would be wrong and would break ACK-5. The job-id binding is the correct
		// test at every hop, and this asserts it stays that way.
		key := ackMessageID(t, 13) // minted in ackOriginBus, not in ackPeerBus
		ob := newRealOutbox(t, ackPeerBus, key)
		rig := newAckRig(t, func(c *AckConfig) { c.Obligations = ob })
		if rec := rig.post(t, ackPeerBus, ackFrame(key, recipient)); rec.Code != http.StatusOK {
			t.Fatalf("a third bus's key acknowledged by the peer we owe got %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a duplicate is 200 with duplicate:true and re-applies nothing", func(t *testing.T) {
		key := ackMessageID(t, 14)
		ob := newRealOutbox(t, ackPeerBus, key)
		rig := newAckRig(t, func(c *AckConfig) { c.Obligations = ob })
		rig.settled.duplicate = true
		rec := rig.post(t, ackPeerBus, ackFrame(key, recipient))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", rec.Code)
		}
		var body PeerAckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !body.Accepted || !body.Duplicate {
			t.Errorf("body = %+v, want accepted:true duplicate:true", body)
		}
		if got := rig.h.Stats().Duplicates; got != 1 {
			t.Errorf("Stats().Duplicates = %d, want 1", got)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a conflicting terminal is 409, logged, and does NOT disconnect", func(t *testing.T) {
		key := ackMessageID(t, 15)
		ob := newRealOutbox(t, ackPeerBus, key)
		rig := newAckRig(t, func(c *AckConfig) { c.Obligations = ob })
		rig.settled.err = fmt.Errorf("%w: already delivered/AckClass(0), refusing refused/recipient_refused_policy", ErrAckOutcomeConflict)
		rec := rig.post(t, ackPeerBus, ackFrame(key, recipient))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusConflict)
		}
		if got := errorCodeOf(t, rec); got != CodeIdempotencyViolation {
			t.Errorf("code %q, want %q", got, CodeIdempotencyViolation)
		}
		if got := rig.h.Stats().Conflicts; got != 1 {
			t.Errorf("Stats().Conflicts = %d, want 1", got)
		}
		if logs := rig.logs.String(); !strings.Contains(logs, "NOT disconnected") {
			t.Errorf("the conflict log line does not record that nothing was disconnected; log:\n%s", logs)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a durable failure is 503 and NOT a 4xx", func(t *testing.T) {
		key := ackMessageID(t, 16)
		ob := newRealOutbox(t, ackPeerBus, key)
		rig := newAckRig(t, func(c *AckConfig) { c.Obligations = ob })
		rig.settled.err = errors.New("the delivery lifecycle table has no durable log attached")
		rec := rig.post(t, ackPeerBus, ackFrame(key, recipient))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want %d — 'not now', so the terminal outcome is re-offered rather than lost", rec.Code, http.StatusServiceUnavailable)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("an ABSENT protocol version reads as 1", func(t *testing.T) {
		// The only backward-compatible read, and it is exact: version 1 IS this
		// format. A frame written before the field existed is a version-1 frame
		// by definition.
		key := ackMessageID(t, 17)
		ob := newRealOutbox(t, ackPeerBus, key)
		rig := newAckRig(t, func(c *AckConfig) { c.Obligations = ob })
		frame := ackFrame(key, recipient)
		frame.ProtocolVersion = 0
		buf, _ := json.Marshal(frame)
		if strings.Contains(string(buf), "protocol_version") {
			t.Fatalf("an unset version encoded a protocol_version key; it must be omitted entirely: %s", buf)
		}
		if rec := rig.post(t, ackPeerBus, string(buf)); rec.Code != http.StatusOK {
			t.Fatalf("a versionless frame got %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		got, _ := rig.settled.last()
		if got.Ack.ProtocolVersion != 1 {
			t.Errorf("a versionless frame resolved to version %d, want 1", got.Ack.ProtocolVersion)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("an UNRECOGNISED version is REFUSED, never defaulted", func(t *testing.T) {
		// parseOutboxState's posture, and the stakes are higher: this frame
		// carries a TERMINAL outcome and terminal is ABSORBING, so a v2 frame read
		// under v1's rules could write a settlement that can never be corrected.
		key := ackMessageID(t, 18)
		for _, version := range []int{2, 3, 42, -1, 1 << 30} {
			ob := newRealOutbox(t, ackPeerBus, key)
			rig := newAckRig(t, func(c *AckConfig) { c.Obligations = ob })
			frame := ackFrame(key, recipient)
			frame.ProtocolVersion = version
			rec := rig.post(t, ackPeerBus, frame)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("version %d: status %d, want %d — an unrecognised version must be refused, never guessed at", version, rec.Code, http.StatusBadRequest)
			}
			if got := errorCodeOf(t, rec); got != CodeUnsupportedAckVersion {
				t.Errorf("version %d: code %q, want %q — the code is the entire diagnosis the far-end operator sees, and it must point at the binaries rather than at a malformed field", version, got, CodeUnsupportedAckVersion)
			}
			if rig.settled.count() != 0 {
				t.Errorf("version %d reached the durable settle", version)
			}
			// AND THE VERSION IS CHECKED FIRST. Every other field of this frame is
			// legal, so the only thing that could have refused it is the version;
			// a frame whose version we cannot read must not have its other fields
			// interpreted under rules that may not apply.
			if got := rig.table.lookups.Load(); got != 0 {
				t.Errorf("version %d cost %d obligation lookups; the version governs the reading of every field and is resolved first", version, got)
			}
		}
	})

	// -----------------------------------------------------------------------
	t.Run("emitted_at is required, and is never persisted", func(t *testing.T) {
		key := ackMessageID(t, 19)
		ob := newRealOutbox(t, ackPeerBus, key)
		rig := newAckRig(t, func(c *AckConfig) { c.Obligations = ob })
		frame := ackFrame(key, recipient)
		frame.EmittedAtUnixMilli = 0
		if rec := rig.post(t, ackPeerBus, frame); rec.Code != http.StatusBadRequest {
			t.Fatalf("a frame with no emitted_at got %d, want %d", rec.Code, http.StatusBadRequest)
		}
		// It is carried as PROVENANCE and nothing decides on it. The durable
		// record's clock is THIS bus's — see ack.Store — and the assertion that
		// it never becomes an input lives with the settle callback, which
		// receives it and does not pass it on.
		ok := newAckRig(t, func(c *AckConfig) { c.Obligations = newRealOutbox(t, ackPeerBus, key) })
		if rec := ok.post(t, ackPeerBus, ackFrame(key, recipient)); rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", rec.Code)
		}
		got, _ := ok.settled.last()
		if got.Ack.EmittedAtUnixMilli != 1_700_000_000_000 {
			t.Errorf("emitted_at = %d, want it carried verbatim as provenance", got.Ack.EmittedAtUnixMilli)
		}
	})
}

// ---------------------------------------------------------------------------
// 3. Bounds and the round trip
// ---------------------------------------------------------------------------

// TestMaxAckBytesFitsAMaximumFrame pins the derivation in MaxAckBytes's comment
// against the REAL encoder. A bound nothing checks is a description.
func TestMaxAckBytesFitsAMaximumFrame(t *testing.T) {
	widest := widestLegalAckFrame()

	// IT MUST BE A FRAME THE ROUTE WOULD ACTUALLY ACCEPT. Without this the
	// fixture can drift into an illegal shape and still "prove" a bound — which
	// is what happened: an earlier version paired `undeliverable` with a
	// recipient class and an attestation, both of which the validator refuses.
	// Only the ids are placeholders, and they are validated later by
	// AuthorizePeerAck rather than here, so this call exercises everything
	// ValidatePeerAckRequest owns.
	if _, err := ValidatePeerAckRequest(widest, AckSurfacePeer); err != nil {
		t.Fatalf("the \"widest legal frame\" is not legal: %v — a bound witnessed by a frame the route would refuse witnesses nothing", err)
	}

	buf, err := json.Marshal(widest)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(buf) > MaxAckBytes {
		t.Fatalf("the widest legal ACK frame encodes to %d bytes, over the %d cap; a legal frame must never be refused by this bound", len(buf), MaxAckBytes)
	}
	// AND THE HEADROOM IS REAL RATHER THAN ACCIDENTAL. If the widest legal frame
	// ever approaches the cap, the derivation in the comment has drifted from the
	// struct and the next field added would silently break a legal peer.
	if len(buf)*2 > MaxAckBytes {
		t.Errorf("the widest legal ACK frame is %d bytes against a %d cap — under 2x headroom. Re-derive the bound in MaxAckBytes's comment", len(buf), MaxAckBytes)
	}
	// The cap must also be far below the relay envelope's: an ACK has no body and
	// no recipient list, so sharing MaxRelayBytes would let an ACK-shaped stream
	// cost orders of magnitude more than an ACK can legally cost.
	if MaxAckBytes >= MaxRelayBytes {
		t.Errorf("MaxAckBytes (%d) is not below MaxRelayBytes (%d)", MaxAckBytes, MaxRelayBytes)
	}
}

// TestPeerAckRoundTripsThroughTheClient drives the SENDING half against the
// receiving half over a REAL TLS server, by actually CALLING Client.PeerAck.
//
// AN EARLIER VERSION OF THIS TEST RE-IMPLEMENTED PeerAck'S BODY INLINE and only
// called the real method on the failure path — so the version stamping, the
// egress byte cap, the PeerRefusedError mapping and the response decode were all
// unexercised while the doc comment claimed they were proved. A reviewer caught
// it. httptest.NewTLSServer plus srv.Client() satisfies peerURL's https-only
// origin rule (invariant 11) without weakening it, which is what makes the real
// call reachable at all.
func TestPeerAckRoundTripsThroughTheClient(t *testing.T) {
	key := ackMessageID(t, 21)
	recipient := ackAgentID(t, ackLocalBus, "bravo")

	rig := newAckRig(t, nil)
	rig.table.owe(ackPeerBus, key)

	// THE RAW REQUEST BODY IS CAPTURED, and that is the only way this test can
	// say anything about the version at all. See the assertion below.
	var gotPath string
	var gotBody []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(io.LimitReader(r.Body, MaxAckBytes+1))
		r.Body = io.NopCloser(bytes.NewReader(gotBody))
		rig.h.ServeAuthenticated(w, r, ackPeerBus)
	}))
	defer srv.Close()

	c := &Client{busID: ackOriginBus, httpClient: srv.Client(), log: logging.New(io.Discard, logging.LevelError)}

	// THE VERSION IS DELIBERATELY LEFT UNSET. PeerAck must stamp it, which is
	// what makes an ABSENT version on the wire unambiguously mean "written before
	// the field existed" rather than "this sender chose not to say".
	frame := ackFrame(key, recipient)
	frame.ProtocolVersion = 0

	resp, err := c.PeerAck(context.Background(), srv.URL, frame)
	if err != nil {
		t.Fatalf("Client.PeerAck: %v", err)
	}
	if !resp.Accepted || resp.Duplicate {
		t.Errorf("PeerAck returned %+v, want accepted:true duplicate:false", resp)
	}
	// The client appends the path ITSELF, so a mismatch is a typo no status
	// assertion would reveal.
	if gotPath != PeerAckPath {
		t.Errorf("PeerAck POSTed to %q, want %q", gotPath, PeerAckPath)
	}
	if _, ok := rig.settled.last(); !ok {
		t.Fatal("nothing reached the durable settle")
	}

	// THE VERSION IS ASSERTED ON THE WIRE, FROM THE RAW BYTES, AND IT MUST NEVER
	// BE ASSERTED ON THE RESOLVED ValidatedPeerAck.
	//
	// An earlier version of this test checked `got.Ack.ProtocolVersion ==
	// AckWireVersion` and a reviewer proved it VACUOUS: an ABSENT version
	// resolves to 1 and AckWireVersion IS 1, so resolution is precisely what
	// erases the distinction the assertion claims to make. Mutating
	// Client.PeerAck to `req.ProtocolVersion = 0` — the client actively
	// STRIPPING the version from every frame this bus emits, the exact inverse
	// of the documented invariant — left that assertion and the whole package
	// green. Nothing else in the repository observes what PeerAck puts on the
	// wire.
	//
	// So: the key must be PRESENT in the encoded body, and it must carry the
	// version. `omitempty` means a stripped version produces NO KEY AT ALL,
	// which is what makes presence the load-bearing half.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("the body PeerAck sent is not an object: %v (%s)", err, gotBody)
	}
	rawVersion, present := raw["protocol_version"]
	if !present {
		t.Fatalf("PeerAck put NO protocol_version on the wire (body %s). Every frame this bus emits must declare the version it speaks — that is what makes an ABSENT version unambiguously mean \"written before the field existed\" rather than \"this sender chose not to say\"", gotBody)
	}
	if string(rawVersion) != fmt.Sprintf("%d", AckWireVersion) {
		t.Errorf("PeerAck put protocol_version=%s on the wire, want %d", rawVersion, AckWireVersion)
	}

	// A DUPLICATE ROUND-TRIPS AS duplicate:true — the response decode is real,
	// not a zero value the client invented.
	rig.settled.duplicate = true
	dup, err := c.PeerAck(context.Background(), srv.URL, frame)
	if err != nil {
		t.Fatalf("Client.PeerAck (duplicate): %v", err)
	}
	if !dup.Accepted || !dup.Duplicate {
		t.Errorf("the duplicate round-trip returned %+v, want accepted:true duplicate:true", dup)
	}
	rig.settled.duplicate = false

	// A REFUSAL BECOMES A PeerRefusedError CARRYING THE STABLE CODE, and its
	// Retriable verdict is the one that decides whether a terminal outcome is
	// re-offered or ABANDONED.
	t.Run("a refusal maps onto PeerRefusedError with the stable code", func(t *testing.T) {
		unowed := ackFrame(ackMessageID(t, 4242), recipient)
		_, err := c.PeerAck(context.Background(), srv.URL, unowed)
		var refused *PeerRefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("err = %v (%T), want *PeerRefusedError", err, err)
		}
		if refused.StatusCode != http.StatusConflict {
			t.Errorf("StatusCode = %d, want %d", refused.StatusCode, http.StatusConflict)
		}
		if refused.Code != CodeIdempotencyViolation {
			t.Errorf("Code = %q, want %q — an unrecognised code here means peerErrorCode's allow-list is missing an arm, and the sending operator reads \"unrecognised error code\" instead of the reason",
				refused.Code, CodeIdempotencyViolation)
		}
		if refused.Retriable() {
			t.Error("a 409 reported itself retriable; the sender would re-offer a settled refusal for its whole horizon")
		}
	})

	// THE VERSION REFUSAL'S CODE SURVIVES THE ROUND TRIP. This is the code a
	// partial rollout produces, and failJSON puts ONLY the code on the wire — so
	// if peerErrorCode does not recognise it, this string IS the entire, and
	// entirely useless, diagnosis the far-end operator ever gets.
	t.Run("an unsupported version reaches the sender as its own code", func(t *testing.T) {
		// PeerAck always stamps AckWireVersion, so a FUTURE version can only be
		// produced by a peer running a newer binary. The responder half is driven
		// directly, and the sender half is the allow-list assertion below — which
		// is the half that was actually missing.
		bumped := ackFrame(key, recipient)
		bumped.ProtocolVersion = 99
		rec := rig.post(t, ackPeerBus, bumped)
		if got := errorCodeOf(t, rec); got != CodeUnsupportedAckVersion {
			t.Fatalf("the responder answered %q, want %q", got, CodeUnsupportedAckVersion)
		}
		body, _ := json.Marshal(ErrorBody{Error: CodeUnsupportedAckVersion})
		if got := peerErrorCode(body); got != CodeUnsupportedAckVersion {
			t.Errorf("peerErrorCode(%s) = %q; the code must be in the allow-list or a partial rollout is undiagnosable from the sending side", body, got)
		}
	})

	// AND THE CLIENT REFUSES A PLAINTEXT PEER (invariant 11): peerURL is the one
	// place the https-only origin rule lives, and PeerAck goes through it.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer plain.Close()
	if _, err := c.PeerAck(context.Background(), plain.URL, frame); err == nil {
		t.Error("Client.PeerAck accepted an http:// peer; invariant 11 has no plaintext peer transport")
	}
}
