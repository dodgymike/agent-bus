package client

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubBus is a fake agent-bus for the AUTHENTICATED surface — the messaging and
// polling routes of CLI-3/4/5.
//
// It exists because every one of those routes authenticates, so a test that
// wants to assert one thing about `POST /v1/send` still has to serve the whole
// credential handshake first: `POST /v1/session/begin` issues a token, the
// client SIGNS THE TOKEN THE BUS CHOSE (Ed25519 over SessionSigningContext +
// token), and `POST /v1/session/complete` verifies it against the enrolled
// public key. This does that for real — ed25519.Verify, not a stub that waves
// the signature through — so the tests below drive the same code path a live
// bus would, and a regression in the handshake fails them rather than hiding.
//
// It is FACTORED OUT rather than copied into each test file for the obvious
// reason: four hand-rolled copies of a signature-verifying handler is four
// places for the handshake to drift.
type stubBus struct {
	t   *testing.T
	srv *httptest.Server

	// Dir is a credential store directory seeded with one usable identity, so
	// a Client built against this bus can resolve a credential and sign.
	Dir string

	// AgentID is the fully-qualified id of that identity (invariant 2).
	AgentID string

	mu       sync.Mutex
	tokens   map[string]bool
	issued   int
	requests []stubRequest
}

// stubRequest is one call the stub bus saw, captured before it was dispatched.
type stubRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
	Bearer string
}

// JSON decodes the recorded request body as a generic object. It returns nil
// when the body was empty or not an object.
func (r stubRequest) JSON() map[string]interface{} {
	var m map[string]interface{}
	if err := json.Unmarshal(r.Body, &m); err != nil {
		return nil
	}
	return m
}

// newStubBus stands up a bus that answers the session handshake itself and
// hands every other route to route.
//
// route is called with the ORIGINAL request body still readable, and only after
// the bearer token has been checked — an unauthenticated call is answered 401
// by the stub, exactly as the real mux's default-deny middleware does.
func newStubBus(t *testing.T, agentID string, route http.HandlerFunc) *stubBus {
	t.Helper()

	b := &stubBus{t: t, Dir: t.TempDir(), AgentID: agentID, tokens: map[string]bool{}}
	cred, pub := newTestCredential(t, agentID)

	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw []byte
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		b.mu.Lock()
		b.requests = append(b.requests, stubRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Body:   append([]byte(nil), raw...),
			Bearer: bearer,
		})
		b.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))

		switch r.URL.Path {
		case routeSessionBegin:
			b.mu.Lock()
			b.issued++
			token := fmt.Sprintf("stub-session-token-%d", b.issued)
			b.tokens[token] = true
			b.mu.Unlock()
			stubWriteJSON(w, http.StatusOK, sessionBeginResponse{
				AgentID:            agentID,
				Token:              token,
				ChallengeExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case routeSessionComplete:
			var body sessionCompleteRequest
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("stub bus: session/complete body is not JSON: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sig, err := base64.StdEncoding.DecodeString(body.Signature)
			if err != nil {
				t.Errorf("stub bus: signature is not base64: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !ed25519.Verify(pub, []byte(SessionSigningContext+body.Token), sig) {
				t.Errorf("stub bus: signature over %q did not verify against the enrolled public key", body.Token)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			stubWriteJSON(w, http.StatusOK, sessionCompleteResponse{
				AgentID:             agentID,
				ExpiresAt:           time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
				LifetimeSeconds:     3600,
				RefreshAfterSeconds: 2700,
			})
		default:
			b.mu.Lock()
			known := b.tokens[bearer]
			b.mu.Unlock()
			if !known {
				stubWriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "no such session"})
				return
			}
			route(w, r)
		}
	}))
	t.Cleanup(b.srv.Close)

	s, err := OpenStore(b.Dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred.BusURL = b.srv.URL
	if err := s.PromotePending("", cred, true); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	return b
}

// URL is the bus base URL.
func (b *stubBus) URL() string { return b.srv.URL }

// client builds a Client pointed at this bus, using the seeded credential
// store. mutate may adjust the config before New is called.
func (b *stubBus) client(t *testing.T, mutate func(*Config)) *Client {
	t.Helper()
	cfg := Config{BusURL: b.srv.URL, IdentityDir: b.Dir}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// calls returns every recorded request for one path, in order.
func (b *stubBus) calls(path string) []stubRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]stubRequest, 0, len(b.requests))
	for _, r := range b.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// stubWriteJSON writes a JSON body with a status.
func stubWriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// stubMessage builds one wire-shaped message with a real content hash, so a
// test's fixtures look like what the bus actually emits.
func stubMessage(seq uint64, from string, body string) Message {
	sum := sha256.Sum256([]byte(body))
	return Message{
		MessageID:     fmt.Sprintf("bus-x-%d", seq),
		Seq:           seq,
		From:          from,
		Broadcast:     false,
		To:            []string{"bus-x.agent-1"},
		BusPath:       []string{"bus-x"},
		SentAt:        time.Now().UTC().Format(time.RFC3339Nano),
		Size:          len(body),
		ContentSHA256: hex.EncodeToString(sum[:]),
		Body:          []byte(body),
	}
}

// stubSendResponse is the wire shape of an accepted send or broadcast. It
// mirrors httpapi.SendResponseBody (CONTRACTS-HTTP.md, "Messaging routes") and
// deliberately omits `replayed` and `idempotency_key`, which the bus does not
// send in the body.
type stubSendResponse struct {
	MessageID     string   `json:"message_id"`
	Seq           uint64   `json:"seq"`
	From          string   `json:"from"`
	Broadcast     bool     `json:"broadcast"`
	To            []string `json:"to"`
	SentAt        string   `json:"sent_at"`
	ContentSHA256 string   `json:"content_sha256"`
}

func stubAccepted(seq uint64, from string, to []string, broadcast bool, body []byte) stubSendResponse {
	sum := sha256.Sum256(body)
	return stubSendResponse{
		MessageID:     fmt.Sprintf("bus-x-%d", seq),
		Seq:           seq,
		From:          from,
		Broadcast:     broadcast,
		To:            to,
		SentAt:        time.Now().UTC().Format(time.RFC3339Nano),
		ContentSHA256: hex.EncodeToString(sum[:]),
	}
}
