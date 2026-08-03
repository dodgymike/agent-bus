package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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

	"github.com/dodgymike/agent-bus/client"
)

// Route paths, pinned as test literals. The CLI reaches them only through the
// client package, which pins its own copies (client/messages.go) — so a
// divergence between the two shows up here as a 404 rather than as a silently
// passing test.
const (
	stubRouteSessionBegin    = "/v1/session/begin"
	stubRouteSessionComplete = "/v1/session/complete"
	stubRouteAgents          = "/v1/agents"
	stubRouteSend            = "/v1/send"
	stubRouteBroadcast       = "/v1/broadcast"
	stubRouteWait            = "/v1/wait"
)

// stubBus is a fake agent-bus for the CLI's authenticated subcommands.
//
// Every messaging route authenticates, so a test of `busctl send` still has to
// serve the whole credential handshake: session/begin issues a token, the
// client SIGNS THE TOKEN THE BUS CHOSE (Ed25519 over
// client.SessionSigningContext + token), session/complete verifies it against
// the enrolled public key. This does that for real — ed25519.Verify, not a
// handler that waves it through — so these tests drive the same path a live bus
// would.
//
// It is factored out rather than copied into watch/send/agents so the handshake
// has one definition to drift from.
type stubBus struct {
	t   *testing.T
	srv *httptest.Server

	// Dir is a credential store directory seeded with one usable identity.
	Dir string

	// AgentID is that identity's fully-qualified id (invariant 2).
	AgentID string

	mu       sync.Mutex
	tokens   map[string]bool
	issued   int
	requests []stubRequest
}

// stubRequest is one call the stub bus saw, captured before dispatch.
type stubRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

// JSON decodes the recorded body as a generic object, or nil.
func (r stubRequest) JSON() map[string]interface{} {
	var m map[string]interface{}
	if err := json.Unmarshal(r.Body, &m); err != nil {
		return nil
	}
	return m
}

// stubSessionBeginResponse etc. mirror the three auth wire shapes.
type stubSessionBeginResponse struct {
	AgentID            string `json:"agent_id"`
	Token              string `json:"token"`
	ChallengeExpiresAt string `json:"challenge_expires_at"`
}

type stubSessionCompleteRequest struct {
	Token     string `json:"token"`
	Signature string `json:"signature"`
}

type stubSessionCompleteResponse struct {
	AgentID             string `json:"agent_id"`
	ExpiresAt           string `json:"expires_at"`
	LifetimeSeconds     int    `json:"lifetime_seconds"`
	RefreshAfterSeconds int    `json:"refresh_after_seconds"`
}

// newStubBus stands up a bus that answers the session handshake itself and
// hands every other route to route, with the original body still readable.
func newStubBus(t *testing.T, agentID string, route http.HandlerFunc) *stubBus {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a test key pair: %v", err)
	}

	b := &stubBus{t: t, Dir: t.TempDir(), AgentID: agentID, tokens: map[string]bool{}}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw []byte
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
		}
		b.mu.Lock()
		b.requests = append(b.requests, stubRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Body:   append([]byte(nil), raw...),
		})
		b.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))

		switch r.URL.Path {
		case stubRouteSessionBegin:
			b.mu.Lock()
			b.issued++
			token := fmt.Sprintf("stub-session-token-%d", b.issued)
			b.tokens[token] = true
			b.mu.Unlock()
			stubWriteJSON(w, http.StatusOK, stubSessionBeginResponse{
				AgentID:            agentID,
				Token:              token,
				ChallengeExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case stubRouteSessionComplete:
			var body stubSessionCompleteRequest
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("stub bus: session/complete body is not JSON: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sig, derr := base64.StdEncoding.DecodeString(body.Signature)
			if derr != nil {
				t.Errorf("stub bus: signature is not base64: %v", derr)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !ed25519.Verify(pub, []byte(client.SessionSigningContext+body.Token), sig) {
				t.Errorf("stub bus: signature over %q did not verify against the enrolled public key", body.Token)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			stubWriteJSON(w, http.StatusOK, stubSessionCompleteResponse{
				AgentID:             agentID,
				ExpiresAt:           time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
				LifetimeSeconds:     3600,
				RefreshAfterSeconds: 2700,
			})
		default:
			bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
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

	s, err := client.OpenStore(b.Dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	busID := agentID
	if i := strings.Index(agentID, "."); i > 0 {
		busID = agentID[:i]
	}
	cred := client.Credential{
		Identity: client.Identity{
			AgentID:    agentID,
			BusID:      busID,
			Name:       "agent",
			BusURL:     b.srv.URL,
			PublicKey:  base64.StdEncoding.EncodeToString(pub),
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
		PrivateKeySeed: base64.StdEncoding.EncodeToString(priv.Seed()),
	}
	if err := s.PromotePending("", cred, true); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	return b
}

// URL is the bus base URL.
func (b *stubBus) URL() string { return b.srv.URL }

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

// cliResult is one busctl invocation's observable output.
type cliResult struct {
	Code   int
	Stdout string
	Stderr string
}

// args prefixes the global flags that point busctl at this stub bus. --identity
// is ALWAYS passed: without it the CLI would fall back to DefaultIdentityDir
// and a test would read and write the developer's real credential store.
func (b *stubBus) args(rest ...string) []string {
	return append([]string{"--identity", b.Dir, "--bus", b.srv.URL}, rest...)
}

// run drives the CLI the way the process does, with the TTY answers supplied
// explicitly so the TTY-dependent behaviour of `watch` and `send` is reachable
// from a test that owns no terminal.
func (b *stubBus) run(t *testing.T, stdin string, stdoutIsTTY, stdinIsTTY bool, rest ...string) cliResult {
	t.Helper()
	return b.runCtx(t, context.Background(), stdin, stdoutIsTTY, stdinIsTTY, rest...)
}

func (b *stubBus) runCtx(t *testing.T, ctx context.Context, stdin string, stdoutIsTTY, stdinIsTTY bool, rest ...string) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runWithTTY(ctx, b.args(rest...), strings.NewReader(stdin), &stdout, &stderr, emptyEnv, stdoutIsTTY, stdinIsTTY)
	return cliResult{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

// stubWriteJSON writes a JSON body with a status.
func stubWriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// stubMessage builds one wire-shaped message with a real content hash.
func stubMessage(seq uint64, from, to, body string) client.Message {
	sum := sha256.Sum256([]byte(body))
	return client.Message{
		MessageID:     fmt.Sprintf("bus-x-%d", seq),
		Seq:           seq,
		From:          from,
		To:            []string{to},
		BusPath:       []string{"bus-x"},
		SentAt:        time.Now().UTC().Format(time.RFC3339Nano),
		Size:          len(body),
		ContentSHA256: hex.EncodeToString(sum[:]),
		Body:          []byte(body),
	}
}

// stubSendResponse mirrors httpapi.SendResponseBody. It deliberately omits
// `replayed` and `idempotency_key`: the bus does not send those in the body
// (one is a header, the other is the client's own).
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
