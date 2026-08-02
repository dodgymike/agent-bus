package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestCredential builds a Credential with a real Ed25519 key pair, so a
// fake bus can verify signatures for real.
func newTestCredential(t *testing.T, agentID string) (Credential, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a test key pair: %v", err)
	}
	cred := Credential{
		Identity: Identity{
			AgentID:    agentID,
			BusID:      "bus-x",
			Name:       "agent",
			PublicKey:  base64.StdEncoding.EncodeToString(pub),
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
		PrivateKeySeed: base64.StdEncoding.EncodeToString(priv.Seed()),
	}
	return cred, pub
}

// TestSessionHandshakeSignsWithEnrolledKeyAndCachesIt drives a real two-step
// session handshake against a fake bus that VERIFIES the signature with
// ed25519.Verify, checks SessionInfo never carries the token, and checks a
// second EnsureSession call reuses the cached session instead of beginning a
// new one.
func TestSessionHandshakeSignsWithEnrolledKeyAndCachesIt(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred, pub := newTestCredential(t, "bus-x.agent-1")

	var mu sync.Mutex
	beginCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case routeSessionBegin:
			mu.Lock()
			beginCount++
			n := beginCount
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(sessionBeginResponse{
				AgentID:            cred.AgentID,
				Token:              fmt.Sprintf("challenge-token-%d", n),
				ChallengeExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case routeSessionComplete:
			b, _ := io.ReadAll(r.Body)
			var body sessionCompleteRequest
			if err := json.Unmarshal(b, &body); err != nil {
				t.Errorf("session/complete body is not JSON: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sig, err := base64.StdEncoding.DecodeString(body.Signature)
			if err != nil {
				t.Errorf("signature is not base64: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !ed25519.Verify(pub, []byte(SessionSigningContext+body.Token), sig) {
				t.Errorf("signature over %q did not verify against the enrolled public key", body.Token)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(sessionCompleteResponse{
				AgentID:             cred.AgentID,
				ExpiresAt:           time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
				LifetimeSeconds:     3600,
				RefreshAfterSeconds: 2700,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cred.BusURL = srv.URL
	if err := s.PromotePending("", cred, true); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}

	c, err := New(Config{BusURL: srv.URL, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	info, err := c.EnsureSession(context.Background())
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if info.AgentID != cred.AgentID {
		t.Fatalf("SessionInfo.AgentID = %q, want %q", info.AgentID, cred.AgentID)
	}

	body, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal SessionInfo: %v", err)
	}
	if strings.Contains(string(body), "challenge-token") {
		t.Fatalf("SessionInfo JSON leaks the session token: %s", body)
	}

	if _, err := c.EnsureSession(context.Background()); err != nil {
		t.Fatalf("second EnsureSession: %v", err)
	}
	mu.Lock()
	got := beginCount
	mu.Unlock()
	if got != 1 {
		t.Fatalf("session/begin was called %d times, want 1 — a cached, still-usable session must be reused", got)
	}
}

// TestSessionBeginRejectsOverlongChallengeToken checks validateChallengeToken
// bounds the token BEFORE it is signed: a bus that returns a token longer
// than the protocol allows must be refused with KindServer, and — the part
// that matters — session/complete must NEVER be called, because signing an
// attacker-chosen blob of arbitrary length is exactly what this check exists
// to prevent.
func TestSessionBeginRejectsOverlongChallengeToken(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred, _ := newTestCredential(t, "bus-x.agent-1")

	var completeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case routeSessionBegin:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(sessionBeginResponse{
				AgentID:            cred.AgentID,
				Token:              strings.Repeat("a", maxChallengeTokenLen+1),
				ChallengeExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case routeSessionComplete:
			atomic.AddInt32(&completeCount, 1)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(sessionCompleteResponse{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cred.BusURL = srv.URL
	if err := s.PromotePending("", cred, true); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	c, err := New(Config{BusURL: srv.URL, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.EnsureSession(context.Background())
	if err == nil {
		t.Fatalf("EnsureSession against an over-long challenge token = nil error, want one")
	}
	if KindOf(err) != KindServer {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindServer)
	}
	if got := atomic.LoadInt32(&completeCount); got != 0 {
		t.Fatalf("session/complete was called %d times, want 0 — an invalid token must never be signed and presented", got)
	}
}

// TestSessionBeginRejectsBadCharacterInChallengeToken checks a challenge token
// containing a byte outside the base64url alphabet is refused the same way,
// and session/complete is never reached.
func TestSessionBeginRejectsBadCharacterInChallengeToken(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred, _ := newTestCredential(t, "bus-x.agent-1")

	var completeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case routeSessionBegin:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(sessionBeginResponse{
				AgentID:            cred.AgentID,
				Token:              "not valid base64url!! \x1b",
				ChallengeExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case routeSessionComplete:
			atomic.AddInt32(&completeCount, 1)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(sessionCompleteResponse{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cred.BusURL = srv.URL
	if err := s.PromotePending("", cred, true); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	c, err := New(Config{BusURL: srv.URL, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.EnsureSession(context.Background())
	if err == nil {
		t.Fatalf("EnsureSession against a challenge token with an invalid character = nil error, want one")
	}
	if KindOf(err) != KindServer {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindServer)
	}
	if got := atomic.LoadInt32(&completeCount); got != 0 {
		t.Fatalf("session/complete was called %d times, want 0 — an invalid token must never be signed and presented", got)
	}
}

// TestSessionCompleteAgentIDMismatchIsRejected checks a session/complete
// response naming a DIFFERENT agent than the one that signed the challenge is
// rejected, rather than silently accepted as this identity's session.
func TestSessionCompleteAgentIDMismatchIsRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred, pub := newTestCredential(t, "bus-x.agent-1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case routeSessionBegin:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(sessionBeginResponse{
				AgentID:            cred.AgentID,
				Token:              "challenge-token-1",
				ChallengeExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case routeSessionComplete:
			b, _ := io.ReadAll(r.Body)
			var body sessionCompleteRequest
			_ = json.Unmarshal(b, &body)
			sig, _ := base64.StdEncoding.DecodeString(body.Signature)
			if !ed25519.Verify(pub, []byte(SessionSigningContext+body.Token), sig) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// A response naming a DIFFERENT agent than the one that signed.
			_ = json.NewEncoder(w).Encode(sessionCompleteResponse{
				AgentID:             "bus-x.somebody-else",
				ExpiresAt:           time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
				LifetimeSeconds:     3600,
				RefreshAfterSeconds: 2700,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cred.BusURL = srv.URL
	if err := s.PromotePending("", cred, true); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	c, err := New(Config{BusURL: srv.URL, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.EnsureSession(context.Background())
	if err == nil {
		t.Fatalf("EnsureSession with a mismatched completed.AgentID = nil error, want one")
	}
	if KindOf(err) != KindServer {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindServer)
	}
}

// TestEnsureSessionSingleFlighted checks N goroutines calling EnsureSession
// concurrently on one Client cause EXACTLY ONE /v1/session/begin. Without the
// single-flight gate, every goroutine that finds the cache cold would run its
// own full handshake — N entries burned in the bus's bounded session table
// for one logical caller.
func TestEnsureSessionSingleFlighted(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred, pub := newTestCredential(t, "bus-x.agent-1")

	var mu sync.Mutex
	beginCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case routeSessionBegin:
			mu.Lock()
			beginCount++
			n := beginCount
			mu.Unlock()
			// A small delay widens the race window so concurrent callers are
			// actually overlapping in-flight, not merely serialised by luck.
			time.Sleep(20 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(sessionBeginResponse{
				AgentID:            cred.AgentID,
				Token:              fmt.Sprintf("challenge-token-%d", n),
				ChallengeExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case routeSessionComplete:
			b, _ := io.ReadAll(r.Body)
			var body sessionCompleteRequest
			_ = json.Unmarshal(b, &body)
			sig, _ := base64.StdEncoding.DecodeString(body.Signature)
			if !ed25519.Verify(pub, []byte(SessionSigningContext+body.Token), sig) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(sessionCompleteResponse{
				AgentID:             cred.AgentID,
				ExpiresAt:           time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
				LifetimeSeconds:     3600,
				RefreshAfterSeconds: 2700,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cred.BusURL = srv.URL
	if err := s.PromotePending("", cred, true); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	c, err := New(Config{BusURL: srv.URL, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := c.EnsureSession(context.Background())
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: EnsureSession: %v", i, err)
		}
	}
	mu.Lock()
	got := beginCount
	mu.Unlock()
	if got != 1 {
		t.Fatalf("session/begin was called %d times across %d concurrent EnsureSession calls, want exactly 1", got, n)
	}
}

// TestAuthorizedRequestReHandshakesExactlyOnceOn401 checks authorizedRequest's
// documented behaviour: a 401 on an authenticated call triggers ONE
// re-handshake, and the retried call carries the new bearer token.
func TestAuthorizedRequestReHandshakesExactlyOnceOn401(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred, pub := newTestCredential(t, "bus-x.agent-1")

	var mu sync.Mutex
	beginCount := 0
	protectedCount := 0
	var tokensPresented []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case routeSessionBegin:
			mu.Lock()
			beginCount++
			n := beginCount
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(sessionBeginResponse{
				AgentID:            cred.AgentID,
				Token:              fmt.Sprintf("challenge-token-%d", n),
				ChallengeExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case routeSessionComplete:
			b, _ := io.ReadAll(r.Body)
			var body sessionCompleteRequest
			_ = json.Unmarshal(b, &body)
			sig, _ := base64.StdEncoding.DecodeString(body.Signature)
			if !ed25519.Verify(pub, []byte(SessionSigningContext+body.Token), sig) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(sessionCompleteResponse{
				AgentID:             cred.AgentID,
				ExpiresAt:           time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
				LifetimeSeconds:     3600,
				RefreshAfterSeconds: 2700,
			})
		case "/v1/protected":
			mu.Lock()
			protectedCount++
			n := protectedCount
			tokensPresented = append(tokensPresented, r.Header.Get("Authorization"))
			mu.Unlock()
			if n == 1 {
				// Reject whatever bearer was presented the first time, to
				// force exactly one re-handshake.
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "session expired"})
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cred.BusURL = srv.URL
	if err := s.PromotePending("", cred, true); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}

	c, err := New(Config{BusURL: srv.URL, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.authorizedRequest(context.Background(), request{
		method: http.MethodGet,
		path:   "/v1/protected",
		op:     "protected test",
	}); err != nil {
		t.Fatalf("authorizedRequest: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if beginCount != 2 {
		t.Fatalf("session/begin was called %d times, want 2 (the initial handshake plus exactly one re-handshake)", beginCount)
	}
	if protectedCount != 2 {
		t.Fatalf("/v1/protected was called %d times, want 2 (the failed call plus the retry)", protectedCount)
	}
	if len(tokensPresented) == 2 && tokensPresented[0] == tokensPresented[1] {
		t.Fatalf("the retry presented the SAME bearer token as the rejected call: %v", tokensPresented)
	}
}
