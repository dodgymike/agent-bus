package httpapi_test

// ACK-5 — THE TRANSIT ARM OF POST /v1/ack.
//
// When hub.AcknowledgeDelivery reports a TRANSIT acknowledgement, THE ACK ITSELF
// SETTLES NOTHING ON THIS BUS: the settleable sender-visible row lives at the
// ORIGIN, possibly several hops away, and this route owes one backward hop before
// it may say "accepted". (Since ACK-12-FU-DESTINATION-ROW a relayed INGEST does
// write a destination row here, left `accepted`, which transitAck authorises off
// — but the ack forwards the outcome and never settles that row locally.) The
// three things only the ROUTE can be wrong about are what this file proves:
//
//  1. the hop is taken BEFORE the 200 (invariant 4 end to end — the 200 is not
//     written until the origin has the outcome durably, which is exactly what
//     waiting for TransitAck to return buys, hop by hop);
//  2. a failure is a RETRIABLE "not now" (503 + Retry-After) and never a FINAL
//     4xx, because a 4xx would make the recipient ABANDON an acknowledgement
//     that nothing recorded — lost outright rather than delayed;
//  3. a recipient CANNOT TELL THE TWO PATHS APART. Which bus holds the durable
//     row is a fact about the federation's topology, and §13.3's posture is
//     that a recipient learns the outcome of the message it was handed and
//     nothing else about the federation.
//
// The hub-level state machine is proven by
// hub.TestTransitAcknowledgementBoundary; the propagation itself by
// relay.TestThreeBusAckNackPropagation.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	// atOriginBus and atMiddleBus are the two upstream hops of the A -> B -> C
	// line. THIS bus (msgTestBusID) is C: the terminal bus, where the recipient
	// lives and where the outcome is raised.
	atOriginBus = "atbusorigin"
	atMiddleBus = "atbusmiddle"
)

// atFakeTransit is the seam, counted.
//
// The COUNT is the load-bearing part: "a non-transit acknowledgement never
// reaches the seam" can only be asserted on a counter, and "the hop happened
// before the 200" only means anything if the hop happened at all.
type atFakeTransit struct {
	mu     sync.Mutex
	calls  int
	frames []relay.PeerAckRequest
	err    error
}

func (f *atFakeTransit) TransitAck(_ context.Context, frame relay.PeerAckRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.frames = append(f.frames, frame)
	return f.err
}

func (f *atFakeTransit) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *atFakeTransit) last() (relay.PeerAckRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.frames) == 0 {
		return relay.PeerAckRequest{}, false
	}
	return f.frames[len(f.frames)-1], true
}

func (f *atFakeTransit) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// atRig is one server with everything the transit arm needs, plus the HUB —
// which newAckServer does not return and which is the only way to ingest a
// RELAYED message and so reach the arm at all.
type atRig struct {
	srv  *httpapi.Server
	hub  *hub.Hub
	acks *ack.Store
	log  *bytes.Buffer
}

// newATRig mirrors newAckServer (ack_test.go) — a real WAL, a real auth
// service, a real hub and a real ack.Store, nothing doubled — and adds the two
// things this file needs: the AckTransit seam, and the hub handle.
//
// transit is INTERFACE-TYPED so a caller passing a bare nil produces a genuinely
// nil interface. That matters: a nil *T stored in an interface field is a
// NON-nil interface, and the route's `== nil` gate would then call dutifully
// through it on a bus that does not federate at all. The 501 subtest below
// depends on this being right.
func newATRig(t *testing.T, transit httpapi.AckTransit) *atRig {
	t.Helper()

	dir := t.TempDir()
	lg := &bytes.Buffer{}
	logger := logging.New(lg, logging.LevelDebug)

	walLog, err := wal.Open(wal.LogOptions{Dir: dir, Logger: logger})
	if err != nil {
		t.Fatalf("opening the write-ahead log in %q: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := walLog.Close(); err != nil {
			t.Errorf("closing the write-ahead log: %v", err)
		}
	})

	minter, err := ids.NewAgentIDMinter(msgTestBusID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	roster := auth.NewMemoryRoster()
	svc, err := auth.NewService(auth.Options{Minter: minter, Roster: roster})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}

	acks := ack.NewStore(ack.Options{Logger: logger})
	if err := acks.Attach(walLog); err != nil {
		t.Fatalf("attaching the lifecycle table: %v", err)
	}

	h, err := hub.Open(hub.Options{
		BusID:   msgTestBusID,
		DataDir: filepath.Dir(walLog.Path()),
		Durable: walLog,
		Replay: func(fn func(wal.Committed) error) (wal.Recovered, error) {
			return wal.Replay(walLog.Path(), fn)
		},
		NextIndex: walLog.Recovered().NextIndex,
		Roster:    authRosterView{roster},
		Logger:    logger,
		Acks:      acks,
	})
	if err != nil {
		t.Fatalf("opening the messaging hub: %v", err)
	}

	srv := httpapi.New(httpapi.Options{
		Identity:   testIdentity(msgTestBusID),
		Logger:     logger,
		Durable:    walLog,
		Auth:       svc,
		Hub:        h,
		AckTransit: transit,
	})
	return &atRig{srv: srv, hub: h, acks: acks, log: lg}
}

// atOriginAttestation is the ORIGIN BUS's attestation for sender: WELL-FORMED
// BUT NOT GENUINE, which is the right fidelity because nothing on this path
// verifies one. store.WithRelayOrigin applies exactly three rules on the way to
// disk — attest.Canonicalize must accept it, the signature must be
// signing.SignatureSize bytes, and the subject must BE the message's sender —
// and this satisfies all three.
func atOriginAttestation(sender string) attest.Attestation {
	return attest.Attestation{
		AgentID:            sender,
		MessagingPublicKey: bytes.Repeat([]byte{0xCD}, ed25519.PublicKeySize),
		IssuedAtUnixMilli:  msgTestTimestampMs,
		NotAfterUnixMilli:  msgTestTimestampMs + 3_600_000,
		Signature:          bytes.Repeat([]byte{0xEF}, signing.SignatureSize),
	}
}

// atIngestRelayed puts a RELAYED message addressed to recipient into this bus
// and returns its correlation key — the ORIGIN bus's server-minted message id.
//
// It goes through hub.IngestRelayed because that is what writes the stored bus
// path ORIGIN-FIRST AND ENDING AT THIS BUS. Since ACK-12-FU-DESTINATION-ROW a
// relayed ingest also writes a DESTINATION lifecycle row per recipient, left
// `accepted`; transitAck authorises off that row, and the transit arm forwards
// the outcome and settles the row nowhere.
func atIngestRelayed(t *testing.T, r *atRig, seq uint64, recipient string) string {
	t.Helper()
	key, err := ids.MessageID(atOriginBus, seq)
	if err != nil {
		t.Fatalf("ids.MessageID: %v", err)
	}
	sender, err := ids.AgentID(atOriginBus, "alpha", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	if _, err := r.hub.IngestRelayed(context.Background(), hub.RelayedIngestRequest{
		Sender:             sender,
		Recipients:         []string{recipient},
		Body:               []byte("a message that crossed two buses to get here"),
		OriginMessageID:    key,
		OriginAttestation:  atOriginAttestation(sender),
		BusPath:            []string{atOriginBus, atMiddleBus},
		TimestampUnixMilli: msgTestTimestampMs,
		Signature:          bytes.Repeat([]byte{0xAB}, signing.SignatureSize),
	}); err != nil {
		t.Fatalf("IngestRelayed(%q): %v", key, err)
	}
	return key
}

// TestAckRouteTransitStatuses is the route half of ACK-5.
func TestAckRouteTransitStatuses(t *testing.T) {
	// -----------------------------------------------------------------------
	t.Run("a transit acknowledgement is 200 and INDISTINGUISHABLE from a recorded one", func(t *testing.T) {
		// The two bodies are compared BYTE FOR BYTE, because "indistinguishable"
		// is the property and a field-by-field comparison would miss a field
		// somebody adds to one arm and not the other.
		transit := &atFakeTransit{}
		r := newATRig(t, transit)
		beta := enrolAndAuthenticate(t, r.srv, "beta")
		key := atIngestRelayed(t, r, 7, beta.id)

		rec := authed(t, r.srv, beta, http.MethodPost, httpapi.RouteAck, deliveredFrame(key, beta.id))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/ack (transit) = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
		transitBody := rec.Body.String()

		if transit.count() != 1 {
			t.Fatalf("the transit seam was called %d times, want 1. A 200 written without the hop would silently convert the acknowledgement plane to best-effort, and no test on this bus would notice", transit.count())
		}
		// THE FRAME IS REBUILT FROM THE VALIDATED VALUE, so only fields that
		// passed the closed-set validation can be forwarded — this bus cannot
		// launder an unvalidated byte string to a peer.
		frame, ok := transit.last()
		if !ok {
			t.Fatal("no frame reached the seam")
		}
		if frame.CorrelationKey != key || frame.Recipient != beta.id {
			t.Errorf("the forwarded frame is (%q,%q), want (%q,%q)", frame.CorrelationKey, frame.Recipient, key, beta.id)
		}
		if frame.Outcome != "delivered" || frame.Class != "" {
			t.Errorf("the forwarded frame is %s/%q, want delivered and no class (§5.4)", frame.Outcome, frame.Class)
		}
		if frame.Attestation == nil || len(frame.Attestation.Signature) != signing.SignatureSize {
			t.Errorf("the forwarded frame's attestation is %+v; the recipient's own signature is forwarded VERBATIM and this bus re-signs nothing", frame.Attestation)
		}
		if frame.ProtocolVersion != 0 {
			t.Errorf("the forwarded frame declares protocol_version %d; relay.Client.PeerAck stamps it, and a second assignment would be a second place to update at the next bump", frame.ProtocolVersion)
		}
		// THE TRANSIT ACK SETTLED NOTHING LOCALLY. The relayed INGEST wrote the
		// destination row (ACK-12-FU-DESTINATION-ROW), left `accepted`; the transit
		// ack forwards the outcome and must leave that row untouched — the ORIGIN
		// holds the only settleable row (§13.3).
		if r, ok := r.acks.Lookup(key, beta.id); !ok || r.State != ack.StateAccepted {
			t.Errorf("after a transit ack the destination row is (%+v,%v), want STILL accepted; the transit ack must settle nothing locally", r, ok)
		}

		// ---- THE LOCALLY-RECORDED ARM, on a fresh rig, for comparison. ----
		other := &atFakeTransit{}
		r2 := newATRig(t, other)
		alpha2 := enrolAndAuthenticate(t, r2.srv, "alpha")
		beta2 := enrolAndAuthenticate(t, r2.srv, "beta")
		localKey := sendForAck(t, r2.srv, alpha2, beta2.id, "k-at-local")

		rec2 := authed(t, r2.srv, beta2, http.MethodPost, httpapi.RouteAck, deliveredFrame(localKey, beta2.id))
		if rec2.Code != http.StatusOK {
			t.Fatalf("POST /v1/ack (recorded) = %d, want 200; body %s", rec2.Code, rec2.Body.String())
		}
		if transitBody != rec2.Body.String() {
			t.Errorf("the transit body %s differs from the recorded body %s. A recipient must not be able to tell which bus holds the durable row: that is a fact about the federation's topology, and §13.3's posture is that a recipient learns the outcome of the message it was handed and nothing else",
				transitBody, rec2.Body.String())
		}
		if rec.Header().Get("Retry-After") != "" {
			t.Errorf("a successful transit carried Retry-After %q", rec.Header().Get("Retry-After"))
		}

		// AND THE SEAM IS NEVER CALLED ON THE NON-TRANSIT PATH.
		if got := other.count(); got != 0 {
			t.Errorf("a LOCALLY-ORIGINATED acknowledgement reached the transit seam %d times, want 0. This bus IS the origin of that message; forwarding would send a terminal outcome to a bus that never owed it one", got)
		}
		if r, ok := r2.acks.Lookup(localKey, beta2.id); !ok || r.State != ack.StateDelivered {
			t.Errorf("the locally-recorded arm left row (%+v,%v), want a delivered row; the non-transit path must be byte-identical to before", r, ok)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a failed hop is 503 with Retry-After, and NOBODY is disconnected", func(t *testing.T) {
		transit := &atFakeTransit{}
		transit.setErr(errATUpstreamDown)
		r := newATRig(t, transit)
		beta := enrolAndAuthenticate(t, r.srv, "beta")
		key := atIngestRelayed(t, r, 11, beta.id)

		// A REAL SERVER, because "nobody was disconnected" is a fact about a
		// SOCKET and an httptest.ResponseRecorder has none. Connections opened
		// are counted, so a route that hung up would be caught even though Go's
		// transport transparently redials and the next request would succeed
		// anyway.
		var newConns int64
		var mu sync.Mutex
		hs := httptest.NewUnstartedServer(r.srv)
		hs.Config.ConnState = func(_ net.Conn, st http.ConnState) {
			if st == http.StateNew {
				mu.Lock()
				newConns++
				mu.Unlock()
			}
		}
		hs.Start()
		defer hs.Close()
		client := hs.Client()

		post := func(body string) (*http.Response, []byte) {
			t.Helper()
			req, err := http.NewRequest(http.MethodPost, hs.URL+httpapi.RouteAck, bytes.NewReader([]byte(body)))
			if err != nil {
				t.Fatalf("building the request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+beta.token)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("the request failed at the transport: %v — NO ACK-plane refusal may disconnect (§12, invariant 10)", err)
			}
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(resp.Body)
			return resp, raw
		}

		resp, raw := post(deliveredFrame(key, beta.id))
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("a failed hop = %d, want 503. A 4xx is FINAL, so it would make the recipient ABANDON an acknowledgement that nothing recorded — the outcome would be lost outright rather than delayed (§9.3)", resp.StatusCode)
		}
		if got := resp.Header.Get("Retry-After"); got != "1" {
			t.Errorf("Retry-After = %q, want \"1\"", got)
		}
		// THE UPSTREAM'S VERDICT IS NOT ECHOED. The recipient is told "not now"
		// and learns nothing about which bus refused or whether a row exists
		// anywhere else in the federation (§13.3).
		if bytes.Contains(raw, []byte(atUpstreamSecret)) {
			t.Errorf("the 503 body echoed the upstream's own error: %s", raw)
		}
		// AND THE ACK RECORDED NOTHING, so the identical retry is safe. The
		// destination row the relayed ingest wrote is untouched — still `accepted`
		// — because a failed hop settles nothing.
		if r, ok := r.acks.Lookup(key, beta.id); !ok || r.State != ack.StateAccepted {
			t.Errorf("a failed hop changed the destination row to (%+v,%v), want STILL accepted; the ack settles nothing, so the identical retry is safe", r, ok)
		}

		mu.Lock()
		before := newConns
		mu.Unlock()
		if before < 1 {
			t.Fatal("no connection was observed; the reuse assertion below would be vacuous")
		}

		// THE SECOND REQUEST, on the SAME client. It must succeed AND land on
		// the connection already open.
		transit.setErr(nil)
		resp2, _ := post(deliveredFrame(key, beta.id))
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("the retry after a 503 = %d, want 200", resp2.StatusCode)
		}
		mu.Lock()
		after := newConns
		mu.Unlock()
		if after != before {
			t.Errorf("a NEW connection was opened (%d -> %d): the route dropped the socket over a transit failure. A merely BUGGY client, a de-peered neighbour and an upstream bus that is simply down all reach that line",
				before, after)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a build with NO seam answers 501, and no Retry-After", func(t *testing.T) {
		// nil is passed as an UNTYPED nil, which is a genuinely nil interface.
		// A nil *T here would be a NON-nil interface and the route would call
		// dutifully through it on a bus that does not federate at all.
		r := newATRig(t, nil)
		beta := enrolAndAuthenticate(t, r.srv, "beta")
		key := atIngestRelayed(t, r, 21, beta.id)

		rec := authed(t, r.srv, beta, http.MethodPost, httpapi.RouteAck, deliveredFrame(key, beta.id))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("POST /v1/ack with no transit seam = %d, want 501; body %s", rec.Code, rec.Body.String())
		}
		// NO Retry-After: it is a fact about this BUILD rather than a transient
		// condition, and dressing a permanent refusal as transient is how a
		// client ends up in a retry loop that cannot end.
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Errorf("the 501 carried Retry-After %q; the condition is permanent for this build", got)
		}
		// ONE VOCABULARY, NOT TWO: the same sentence a recipient sees when the
		// LIFECYCLE TABLE is the missing piece, because from its side the fact
		// is identical — this bus cannot record its acknowledgement.
		var body map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding the 501 body %q: %v", rec.Body.String(), err)
		}
		if got, _ := body["error"].(string); got != "delivery acknowledgement is not available on this bus" {
			t.Errorf("the 501 says %q; it must be the same sentence writeAckError's ErrNoAckTable arm uses", got)
		}
		// The 501 settled nothing; the relayed ingest's destination row is
		// untouched — still `accepted`.
		if r, ok := r.acks.Lookup(key, beta.id); !ok || r.State != ack.StateAccepted {
			t.Errorf("the 501 changed the destination row to (%+v,%v), want STILL accepted", r, ok)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("every NON-transit answer is unchanged and never touches the seam", func(t *testing.T) {
		// The refusals the route already made, re-asserted WITH a seam wired,
		// so a regression that routed one of them through the transit arm — and
		// so answered 503 or 501 instead of the uniform 200/`unknown` — is
		// caught here rather than by a peer months later.
		transit := &atFakeTransit{}
		r := newATRig(t, transit)
		alpha := enrolAndAuthenticate(t, r.srv, "alpha")
		beta := enrolAndAuthenticate(t, r.srv, "beta")
		gamma := enrolAndAuthenticate(t, r.srv, "gamma")
		localKey := sendForAck(t, r.srv, alpha, beta.id, "k-at-nontransit")
		relayedKey := atIngestRelayed(t, r, 31, beta.id)

		for _, tc := range []struct {
			name  string
			agent testAgent
			body  string
			want  int
		}{
			{"a recipient acknowledging its own LOCAL message", beta, deliveredFrame(localKey, beta.id), http.StatusOK},
			{"a key that names no message at all", beta, deliveredFrame(msgTestBusID+"-999999", beta.id), http.StatusOK},
			{"a malformed key", beta, deliveredFrame("not-a-message-id", beta.id), http.StatusOK},
			{"an agent acknowledging on somebody else's behalf", gamma, deliveredFrame(localKey, beta.id), http.StatusForbidden},
			{"an agent NOT named in a RELAYED message", gamma, deliveredFrame(relayedKey, gamma.id), http.StatusOK},
			{"a positive terminal carrying a class", beta, ackFrame(localKey, beta.id, "delivered", "recipient_refused_policy", true), http.StatusBadRequest},
			{"a routing outcome from an agent", beta, ackFrame(localKey, beta.id, "undeliverable", "no_route", false), http.StatusBadRequest},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec := authed(t, r.srv, tc.agent, http.MethodPost, httpapi.RouteAck, tc.body)
				if rec.Code != tc.want {
					t.Fatalf("status %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
				}
			})
		}

		if got := transit.count(); got != 0 {
			t.Fatalf("the transit seam was reached %d times by NON-transit acknowledgements, want 0. Every miss that is not a transit must answer byte-identically to before, or the uniform answer §13.3 depends on has been split in two",
				got)
		}
	})
}

// atUpstreamSecret is a distinctive string inside the upstream's error, so the
// "the verdict is not echoed" assertion cannot pass by accident.
const atUpstreamSecret = "UPSTREAMVERDICTXYZZY"

// errATUpstreamDown is what a failed backward hop looks like from the route's
// side: an error it must CLASSIFY (as "not now") and never DISPLAY.
var errATUpstreamDown = &atError{atUpstreamSecret + ": the origin bus refused"}

type atError struct{ msg string }

func (e *atError) Error() string { return e.msg }
