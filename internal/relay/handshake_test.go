package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ids"
)

const (
	localBus = "bus-local"
	peerBus  = "bus-peer"
	thirdBus = "bus-third"
)

// responder is a handshake Handler on a TLS test server, plus the record of
// what its AcceptPeer callback was handed.
type responder struct {
	srv *httptest.Server

	mu       sync.Mutex
	accepted []PeerRoster
	reject   error
}

// newResponder serves a Handler for busID with the given local roster.
//
// The Handler is attached to an httptest server — a TEST harness — and to
// nothing else. Nothing in this package registers it on the product's mux, and
// guards_test.go fails if any other package so much as imports this one.
func newResponder(t *testing.T, busID string, roster []string, cfg func(*Config)) *responder {
	t.Helper()
	r := &responder{}
	c := Config{
		BusID:       busID,
		LocalRoster: func() []string { return append([]string(nil), roster...) },
		AcceptPeer: func(_ context.Context, peer PeerRoster) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.reject != nil {
				return r.reject
			}
			r.accepted = append(r.accepted, peer)
			return nil
		},
	}
	if cfg != nil {
		cfg(&c)
	}
	h, err := NewHandler(c)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r.srv = httptest.NewTLSServer(h)
	t.Cleanup(r.srv.Close)
	return r
}

// rejectWith makes every subsequent AcceptPeer return err. It takes the mutex
// because the handler reads the field from a server goroutine.
func (r *responder) rejectWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reject = err
}

func (r *responder) acceptedRosters() []PeerRoster {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PeerRoster(nil), r.accepted...)
}

// postRaw sends a body the Client would never construct, which is exactly what
// a hostile peer sends.
func (r *responder) postRaw(t *testing.T, contentType string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.srv.URL+PeerEnrollPath, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return r.do(t, req)
}

func (r *responder) postJSON(t *testing.T, payload interface{}) (int, string) {
	t.Helper()
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshalling payload: %v", err)
	}
	return r.postRaw(t, "application/json", buf)
}

func (r *responder) do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	var body ErrorBody
	if resp.StatusCode != http.StatusOK {
		if err := json.Unmarshal(buf, &body); err != nil {
			t.Fatalf("error body %q is not JSON: %v", string(buf), err)
		}
	}
	return resp.StatusCode, body.Error
}

func newInitiator(t *testing.T, busID string, roster []string, srv *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{
		BusID:       busID,
		LocalRoster: func() []string { return append([]string(nil), roster...) },
		HTTPClient:  srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// TestPeerEnrollment is RELAY-1's proof: two buses exchange bus ids and
// fully-qualified rosters, and every way a peer can lie about an id is refused.
func TestPeerEnrollment(t *testing.T) {
	t.Run("exchanges bus ids and fully qualified rosters", func(t *testing.T) {
		remote := newResponder(t, peerBus, []string{peerBus + ".beta-1", peerBus + ".gamma-7"}, nil)
		local := newInitiator(t, localBus, []string{localBus + ".alpha-1"}, remote.srv)

		got, err := local.Enroll(context.Background(), remote.srv.URL, "relay-1-handshake")
		if err != nil {
			t.Fatalf("Enroll: %v", err)
		}

		// The initiator learned the responder's identity and roster...
		if got.BusID != peerBus {
			t.Errorf("peer bus id = %q, want %q", got.BusID, peerBus)
		}
		want := []string{peerBus + ".beta-1", peerBus + ".gamma-7"}
		if len(got.Agents) != len(want) {
			t.Fatalf("peer roster = %v, want %v", got.Agents, want)
		}
		for i, id := range got.Agents {
			if id != want[i] {
				t.Errorf("peer roster[%d] = %q, want %q", i, id, want[i])
			}
			// ...and every id is FULLY QUALIFIED to the peer's bus (invariant 2),
			// which is the whole point of exchanging rosters.
			busPart, _, _, err := ids.ParseAgentID(id)
			if err != nil {
				t.Errorf("peer roster[%d] = %q does not parse as an agent id: %v", i, id, err)
				continue
			}
			if busPart != peerBus {
				t.Errorf("peer roster[%d] = %q is qualified to bus %q, want %q", i, id, busPart, peerBus)
			}
		}

		// ...and the responder learned ours, with the idempotency key carried
		// through for the durable applied-key table (invariant 10).
		accepted := remote.acceptedRosters()
		if len(accepted) != 1 {
			t.Fatalf("responder accepted %d rosters, want 1", len(accepted))
		}
		if accepted[0].BusID != localBus {
			t.Errorf("accepted bus id = %q, want %q", accepted[0].BusID, localBus)
		}
		if len(accepted[0].Agents) != 1 || accepted[0].Agents[0] != localBus+".alpha-1" {
			t.Errorf("accepted roster = %v, want [%s.alpha-1]", accepted[0].Agents, localBus)
		}
		if accepted[0].IdempotencyKey != "relay-1-handshake" {
			t.Errorf("accepted idempotency key = %q, want %q", accepted[0].IdempotencyKey, "relay-1-handshake")
		}
	})

	t.Run("an empty roster is a legal handshake", func(t *testing.T) {
		remote := newResponder(t, peerBus, nil, nil)
		local := newInitiator(t, localBus, nil, remote.srv)

		got, err := local.Enroll(context.Background(), remote.srv.URL, "empty-roster")
		if err != nil {
			t.Fatalf("Enroll: %v", err)
		}
		if got.BusID != peerBus || len(got.Agents) != 0 {
			t.Fatalf("got %+v, want bus %q with an empty roster", got, peerBus)
		}
		if n := len(remote.acceptedRosters()); n != 1 {
			t.Fatalf("responder accepted %d rosters, want 1", n)
		}
	})

	// Every case below is a peer asserting something it is not entitled to
	// assert. In each one the request must be refused AND nothing may reach
	// AcceptPeer — a rejected peer that still got recorded is the bug.
	t.Run("rejects peers that lie about ids", func(t *testing.T) {
		bigRoster := make([]string, MaxRosterAgents+1)
		for i := range bigRoster {
			bigRoster[i] = fmt.Sprintf("%s.a%d-1", peerBus, i)
		}

		cases := []struct {
			name    string
			req     PeerEnrollRequest
			code    string
			because string
		}{
			{
				name:    "claims our bus id",
				req:     PeerEnrollRequest{BusID: localBus, IdempotencyKey: "k1"},
				code:    CodeBusIDCollision,
				because: "a peer asserting our bus id would own our whole namespace",
			},
			{
				name:    "claims our bus id in a different case",
				req:     PeerEnrollRequest{BusID: strings.ToUpper(localBus), IdempotencyKey: "k2"},
				code:    CodeBusIDCollision,
				because: "ASCII case-confusables are removed at the door, as ids.ValidateAgentName does",
			},
			{
				name:    "lists an agent inside our namespace",
				req:     PeerEnrollRequest{BusID: peerBus, IdempotencyKey: "k3", Agents: []string{localBus + ".alpha-1"}},
				code:    CodeBusIDCollision,
				because: "a peer minting ids in our namespace could impersonate our agents to us",
			},
			{
				name:    "lists an agent inside our namespace in a different case",
				req:     PeerEnrollRequest{BusID: peerBus, IdempotencyKey: "k4", Agents: []string{strings.ToUpper(localBus) + ".alpha-1"}},
				code:    CodeBusIDCollision,
				because: "case-folding the bus half must not open the namespace back up",
			},
			{
				name:    "lists a third bus's agent",
				req:     PeerEnrollRequest{BusID: peerBus, IdempotencyKey: "k5", Agents: []string{thirdBus + ".delta-1"}},
				code:    CodeInvalidRoster,
				because: "a peer speaks for its own agents only; transitive federation is not a handshake side effect",
			},
			{
				name:    "sends a bare unqualified name",
				req:     PeerEnrollRequest{BusID: peerBus, IdempotencyKey: "k6", Agents: []string{"beta-1"}},
				code:    CodeInvalidRoster,
				because: "an unqualified id has no bus half and cannot be routed (invariant 2)",
			},
			{
				name:    "sends a malformed id",
				req:     PeerEnrollRequest{BusID: peerBus, IdempotencyKey: "k7", Agents: []string{peerBus + ".beta"}},
				code:    CodeInvalidRoster,
				because: "a bare name with no server-minted suffix is not an agent id (invariant 1)",
			},
			{
				name:    "repeats an id",
				req:     PeerEnrollRequest{BusID: peerBus, IdempotencyKey: "k8", Agents: []string{peerBus + ".beta-1", peerBus + ".beta-1"}},
				code:    CodeInvalidRoster,
				because: "a roster is a set; a duplicate is a bug or an attempt to inflate it",
			},
			{
				name:    "sends an oversized roster",
				req:     PeerEnrollRequest{BusID: peerBus, IdempotencyKey: "k9", Agents: bigRoster},
				code:    CodeRosterTooLarge,
				because: "the length is refused before any per-entry parsing is done",
			},
			{
				name:    "sends a malformed bus id",
				req:     PeerEnrollRequest{BusID: "bus.with.dots", IdempotencyKey: "k10"},
				code:    CodeInvalidBusID,
				because: "'.' is the qualification separator and may not appear in a bus id",
			},
			{
				name:    "omits the idempotency key",
				req:     PeerEnrollRequest{BusID: peerBus},
				code:    CodeInvalidIdempotencyKey,
				because: "peer-enrol is a mutating call and must be safe to retry (invariant 10)",
			},
			{
				name:    "sends an idempotency key with an illegal byte",
				req:     PeerEnrollRequest{BusID: peerBus, IdempotencyKey: "key with spaces"},
				code:    CodeInvalidIdempotencyKey,
				because: "the key alphabet is pinned to the hub's so it can be carried into the applied-key table",
			},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				remote := newResponder(t, localBus, nil, nil)
				status, code := remote.postJSON(t, tc.req)
				if status != http.StatusBadRequest {
					t.Errorf("status = %d, want %d (%s)", status, http.StatusBadRequest, tc.because)
				}
				if code != tc.code {
					t.Errorf("error code = %q, want %q (%s)", code, tc.code, tc.because)
				}
				if n := len(remote.acceptedRosters()); n != 0 {
					t.Errorf("AcceptPeer was called %d times for a rejected handshake, want 0", n)
				}
			})
		}
	})

	t.Run("rejects malformed transport-level requests", func(t *testing.T) {
		remote := newResponder(t, localBus, nil, func(c *Config) { c.MaxRequestBytes = 512 })

		t.Run("wrong method", func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, remote.srv.URL+PeerEnrollPath, nil)
			if err != nil {
				t.Fatalf("building request: %v", err)
			}
			if status, code := remote.do(t, req); status != http.StatusMethodNotAllowed || code != CodeMethodNotAllowed {
				t.Errorf("GET gave %d/%q, want %d/%q", status, code, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
			}
		})

		t.Run("wrong content type", func(t *testing.T) {
			if status, code := remote.postRaw(t, "text/plain", []byte(`{"bus_id":"bus-peer"}`)); status != http.StatusUnsupportedMediaType || code != CodeUnsupportedMediaType {
				t.Errorf("text/plain gave %d/%q, want %d/%q", status, code, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
			}
		})

		t.Run("missing content type", func(t *testing.T) {
			if status, code := remote.postRaw(t, "", []byte(`{"bus_id":"bus-peer"}`)); status != http.StatusUnsupportedMediaType || code != CodeUnsupportedMediaType {
				t.Errorf("no Content-Type gave %d/%q, want %d/%q", status, code, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
			}
		})

		t.Run("unknown field", func(t *testing.T) {
			body := []byte(`{"bus_id":"bus-peer","idempotency_key":"k","agents":[],"trust_me":true}`)
			if status, code := remote.postRaw(t, "application/json", body); status != http.StatusBadRequest || code != CodeInvalidRequest {
				t.Errorf("unknown field gave %d/%q, want %d/%q", status, code, http.StatusBadRequest, CodeInvalidRequest)
			}
		})

		t.Run("trailing data", func(t *testing.T) {
			body := []byte(`{"bus_id":"bus-peer","idempotency_key":"k","agents":[]}{"bus_id":"bus-peer"}`)
			if status, code := remote.postRaw(t, "application/json", body); status != http.StatusBadRequest || code != CodeInvalidRequest {
				t.Errorf("trailing data gave %d/%q, want %d/%q", status, code, http.StatusBadRequest, CodeInvalidRequest)
			}
		})

		t.Run("oversized body", func(t *testing.T) {
			// The bound is enforced on BYTES READ, before any decoding, and
			// without believing Content-Length: this body is syntactically fine
			// and simply too long.
			body := []byte(`{"bus_id":"bus-peer","idempotency_key":"` + strings.Repeat("a", 600) + `"}`)
			if status, code := remote.postRaw(t, "application/json", body); status != http.StatusRequestEntityTooLarge || code != CodePayloadTooLarge {
				t.Errorf("oversized body gave %d/%q, want %d/%q", status, code, http.StatusRequestEntityTooLarge, CodePayloadTooLarge)
			}
		})

		if n := len(remote.acceptedRosters()); n != 0 {
			t.Errorf("AcceptPeer was called %d times for malformed requests, want 0", n)
		}
	})

	t.Run("a declined peer gets 403 and the initiator sees a refusal", func(t *testing.T) {
		remote := newResponder(t, peerBus, nil, nil)
		remote.rejectWith(fmt.Errorf("no invite redeemed: %w", ErrPeerRejected))
		local := newInitiator(t, localBus, nil, remote.srv)

		_, err := local.Enroll(context.Background(), remote.srv.URL, "declined")
		if !errors.Is(err, ErrPeerRefused) {
			t.Fatalf("Enroll error = %v, want one wrapping ErrPeerRefused", err)
		}
		if !strings.Contains(err.Error(), CodePeerRejected) {
			t.Errorf("Enroll error %q does not carry the %q code", err, CodePeerRejected)
		}
	})

	t.Run("a broken responder gets 503, not 403", func(t *testing.T) {
		remote := newResponder(t, peerBus, nil, nil)
		remote.rejectWith(errors.New("the durable peer table is unavailable"))

		status, code := remote.postJSON(t, PeerEnrollRequest{BusID: localBus, IdempotencyKey: "boom"})
		if status != http.StatusServiceUnavailable || code != CodeUnavailable {
			t.Errorf("got %d/%q, want %d/%q: a peer must be able to tell 'not now' from 'never'", status, code, http.StatusServiceUnavailable, CodeUnavailable)
		}
	})

	t.Run("a bus that cannot describe itself refuses the handshake", func(t *testing.T) {
		// A local roster entry from someone else's namespace is a bug on THIS
		// bus. Publishing it would teach the peer to route to an agent we do
		// not have; the handshake fails instead, and no peer is recorded.
		remote := newResponder(t, peerBus, []string{thirdBus + ".ghost-1"}, nil)

		status, code := remote.postJSON(t, PeerEnrollRequest{BusID: localBus, IdempotencyKey: "selfcheck"})
		if status != http.StatusServiceUnavailable || code != CodeUnavailable {
			t.Errorf("got %d/%q, want %d/%q", status, code, http.StatusServiceUnavailable, CodeUnavailable)
		}
		if n := len(remote.acceptedRosters()); n != 0 {
			t.Errorf("AcceptPeer was called %d times although we could not answer, want 0", n)
		}
	})

	// The reply is validated with the same rules as the request: we dialled the
	// peer, which proves we know its address and nothing about who answered.
	t.Run("the initiator validates the responder's reply", func(t *testing.T) {
		cases := []struct {
			name string
			body PeerEnrollResponse
			want error
		}{
			{
				name: "responder claims our bus id",
				body: PeerEnrollResponse{BusID: localBus},
				want: ErrBusIDCollision,
			},
			{
				name: "responder lists an agent in our namespace",
				body: PeerEnrollResponse{BusID: peerBus, Agents: []string{localBus + ".alpha-1"}},
				want: ErrBusIDCollision,
			},
			{
				name: "responder lists a third bus's agent",
				body: PeerEnrollResponse{BusID: peerBus, Agents: []string{thirdBus + ".delta-1"}},
				want: ErrInvalidRoster,
			},
			{
				name: "responder sends a malformed id",
				body: PeerEnrollResponse{BusID: peerBus, Agents: []string{"not-an-agent-id"}},
				want: ErrInvalidRoster,
			},
			{
				name: "responder sends a malformed bus id",
				body: PeerEnrollResponse{BusID: "bus.with.dots"},
				want: ErrInvalidBusID,
			},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(tc.body)
				}))
				defer srv.Close()

				local := newInitiator(t, localBus, nil, srv)
				if _, err := local.Enroll(context.Background(), srv.URL, "hostile-responder"); !errors.Is(err, tc.want) {
					t.Fatalf("Enroll error = %v, want one wrapping %v", err, tc.want)
				}
			})
		}
	})

	t.Run("the initiator refuses a plaintext peer URL", func(t *testing.T) {
		remote := newResponder(t, peerBus, nil, nil)
		local := newInitiator(t, localBus, nil, remote.srv)

		plaintext := strings.Replace(remote.srv.URL, "https://", "http://", 1)
		_, err := local.Enroll(context.Background(), plaintext, "plaintext")
		if err == nil {
			t.Fatal("Enroll over http:// succeeded; there is no plaintext listener (invariant 11)")
		}
		if !strings.Contains(err.Error(), "https") {
			t.Errorf("error %q does not explain that the link must be https", err)
		}
		if n := len(remote.acceptedRosters()); n != 0 {
			t.Errorf("AcceptPeer was called %d times, want 0: nothing should have been sent", n)
		}
	})
}

// TestNewHandlerRejectsIncompleteConfig pins the constructor's refusal to build
// a handler whose failure mode would be silent.
func TestNewHandlerRejectsIncompleteConfig(t *testing.T) {
	roster := func() []string { return nil }
	accept := func(context.Context, PeerRoster) error { return nil }

	cases := []struct {
		name string
		cfg  Config
	}{
		{"no bus id", Config{LocalRoster: roster, AcceptPeer: accept}},
		{"bus id with a dot", Config{BusID: "bus.x", LocalRoster: roster, AcceptPeer: accept}},
		{"no roster provider", Config{BusID: localBus, AcceptPeer: accept}},
		{"no accept callback", Config{BusID: localBus, LocalRoster: roster}},
		{"negative byte cap", Config{BusID: localBus, LocalRoster: roster, AcceptPeer: accept, MaxRequestBytes: -1}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewHandler(tc.cfg); err == nil {
				t.Fatal("NewHandler accepted an incomplete config")
			}
		})
	}
}

// TestNewClientRejectsIncompleteConfig pins the same for the initiator — in
// particular that the http.Client carrying the TLS material is never defaulted.
func TestNewClientRejectsIncompleteConfig(t *testing.T) {
	roster := func() []string { return nil }

	cases := []struct {
		name string
		cfg  ClientConfig
	}{
		{"no bus id", ClientConfig{LocalRoster: roster, HTTPClient: &http.Client{}}},
		{"no roster provider", ClientConfig{BusID: localBus, HTTPClient: &http.Client{}}},
		{"no http client", ClientConfig{BusID: localBus, LocalRoster: roster}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(tc.cfg); err == nil {
				t.Fatal("NewClient accepted an incomplete config")
			}
		})
	}
}

// TestClientRefusesToPublishABrokenLocalRoster proves the initiator applies the
// same self-check the responder does, before anything leaves the process.
func TestClientRefusesToPublishABrokenLocalRoster(t *testing.T) {
	var reached int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	local := newInitiator(t, localBus, []string{thirdBus + ".ghost-1"}, srv)
	if _, err := local.Enroll(context.Background(), srv.URL, "broken-roster"); err == nil {
		t.Fatal("Enroll published a roster from another bus's namespace")
	}
	if reached != 0 {
		t.Errorf("the peer was contacted %d times despite the local roster being invalid, want 0", reached)
	}
}
