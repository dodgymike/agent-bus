package httpapi

// White-box tests for the ONE thing about authMiddleware that cannot be
// observed from outside the package.
//
// AUTH-2 deliberately adds no protected route, so there is no handler anywhere
// that can be asked "what principal did you see?". The only way to prove the
// verified identity actually reaches a downstream handler -- rather than being
// computed, logged and dropped -- is to stand a recording handler behind
// authMiddleware directly. Everything else about the middleware is asserted
// black-box in authmw_test.go, and belongs there.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// mwTestBusID qualifies every agent id these tests expect (invariant 2).
const mwTestBusID = "bus-http-inner"

// mwProbePath is registered nowhere, so a request that reaches the recording
// handler got there through the middleware and nothing else.
const mwProbePath = "/v1/messages/send"

// newMWServer builds a *Server on a real *auth.Service, plus the buffer that
// captures its log at debug level.
func newMWServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()

	minter, err := ids.NewAgentIDMinter(mwTestBusID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	svc, err := auth.NewService(auth.Options{Minter: minter})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}

	var logBuf bytes.Buffer
	srv := New(Options{
		Identity: StaticIdentity(mwTestBusID),
		Logger:   logging.New(&logBuf, logging.LevelDebug),
		Auth:     svc,
	})
	return srv, &logBuf
}

// mwPostJSON issues a JSON POST through the server's full stack and decodes the
// reply into a map.
func mwPostJSON(t *testing.T, srv *Server, path, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decoding %s reply %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, m
}

// mwHandshake runs the real enrol -> begin -> sign -> complete exchange over
// HTTP and returns an ACTIVE token with the server-minted agent id.
func mwHandshake(t *testing.T, srv *Server, name string) (token, agentID string) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating an ed25519 keypair: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	code, body := mwPostJSON(t, srv, RouteEnroll, `{"name":"`+name+`","public_key":"`+pubB64+`","idempotency_key":"idem-`+name+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("enrol status = %d, want 201; body %v", code, body)
	}
	agentID, _ = body["agent_id"].(string)
	if agentID == "" {
		t.Fatalf("enrol returned no agent_id: %v", body)
	}

	code, body = mwPostJSON(t, srv, RouteSessionBegin, `{"agent_id":"`+agentID+`"}`)
	if code != http.StatusOK {
		t.Fatalf("session/begin status = %d, want 200; body %v", code, body)
	}
	token, _ = body["token"].(string)
	if token == "" {
		t.Fatalf("session/begin returned no token: %v", body)
	}

	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(auth.SessionSigningContext+token)))
	code, body = mwPostJSON(t, srv, RouteSessionComplete, `{"token":"`+token+`","signature":"`+sig+`"}`)
	if code != http.StatusOK {
		t.Fatalf("session/complete status = %d, want 200; body %v", code, body)
	}
	return token, agentID
}

// mwRecorder is the handler that stands behind the middleware and reports what
// it was handed.
type mwRecorder struct {
	called    bool
	principal auth.Principal
	ok        bool
	agentID   string
	requestID string
}

func (m *mwRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.called = true
		m.principal, m.ok = PrincipalFromContext(r.Context())
		m.agentID = AgentIDFromContext(r.Context())
		m.requestID = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
}

// TestAuthMiddlewareAttachesThePrincipal proves the verified identity reaches a
// downstream handler, and that nothing else does.
//
// The name is inside TestAuthMiddleware's regex on purpose: the task's
// proof_cmd runs `-run 'TestAuthMiddleware|TestEveryRouteRequiresAuth'`, which
// matches by prefix, so these assertions are part of the proof rather than
// something that quietly never runs.
func TestAuthMiddlewareAttachesThePrincipal(t *testing.T) {
	t.Run("a valid token attaches the fully-qualified agent id", func(t *testing.T) {
		srv, _ := newMWServer(t)
		token, agentID := mwHandshake(t, srv, "alpha")

		rec := &mwRecorder{}
		req := httptest.NewRequest(http.MethodGet, mwProbePath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		srv.authMiddleware(rec.handler()).ServeHTTP(w, req)

		if !rec.called {
			t.Fatalf("the downstream handler was never reached; status %d", w.Code)
		}
		if !rec.ok {
			t.Fatal("PrincipalFromContext reported no principal on an authenticated request")
		}
		// Invariant 2: an agent id is ALWAYS `<bus-id>.<agent-id>`. A handler
		// that received a bare short name could not route across buses, and a
		// handler that received one anyway would be trusting an unqualified id.
		if want := mwTestBusID + ".alpha-1"; rec.agentID != want {
			t.Errorf("AgentIDFromContext = %q, want the fully-qualified %q", rec.agentID, want)
		}
		if rec.agentID != agentID {
			t.Errorf("AgentIDFromContext = %q, but /v1/enroll minted %q; the identity a handler acts on must be the one the server issued", rec.agentID, agentID)
		}
		if rec.principal.AgentID != rec.agentID {
			t.Errorf("principal.AgentID = %q, want %q", rec.principal.AgentID, rec.agentID)
		}
		if rec.principal.ExpiresAt.IsZero() {
			t.Error("principal.ExpiresAt is the zero time; a handler cannot reason about a session that never ends")
		}
		if !rec.principal.ExpiresAt.After(time.Now()) {
			t.Errorf("principal.ExpiresAt = %s is not in the future", rec.principal.ExpiresAt)
		}
	})

	t.Run("an allow-listed route runs the handler with NO principal", func(t *testing.T) {
		srv, _ := newMWServer(t)
		token, _ := mwHandshake(t, srv, "alpha")

		// Even WITH a perfectly good token: no principal is attached on an
		// unauthenticated route, so a handler there cannot come to depend on an
		// identity that is not always present.
		for _, values := range [][]string{nil, {"Bearer " + token}} {
			rec := &mwRecorder{}
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			for _, v := range values {
				req.Header.Add("Authorization", v)
			}
			w := httptest.NewRecorder()
			srv.authMiddleware(rec.handler()).ServeHTTP(w, req)

			if !rec.called {
				t.Fatalf("Authorization %v: the handler on an allow-listed route was not reached; status %d", values, w.Code)
			}
			if rec.ok {
				t.Errorf("Authorization %v: PrincipalFromContext returned ok on an allow-listed route, with principal %+v", values, rec.principal)
			}
			if rec.agentID != "" {
				t.Errorf("Authorization %v: AgentIDFromContext = %q, want the empty string", values, rec.agentID)
			}
		}
	})

	t.Run("a refused request never reaches the handler", func(t *testing.T) {
		srv, _ := newMWServer(t)
		token, _ := mwHandshake(t, srv, "alpha")

		cases := []struct {
			name   string
			values []string
		}{
			{"no header", nil},
			{"malformed header", []string{"Basic dXNlcjpwYXNz"}},
			{"forged token", []string{"Bearer ZmFrZS10b2tlbi1uZXZlci1pc3N1ZWQtYnktdGhpcy1idXM"}},
			{"duplicate valid headers", []string{"Bearer " + token, "Bearer " + token}},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec := &mwRecorder{}
				req := httptest.NewRequest(http.MethodGet, mwProbePath, nil)
				for _, v := range tc.values {
					req.Header.Add("Authorization", v)
				}
				w := httptest.NewRecorder()
				srv.authMiddleware(rec.handler()).ServeHTTP(w, req)

				if w.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", w.Code)
				}
				if rec.called {
					t.Fatal("the downstream handler RAN on a refused request; a 401 must be terminal, not advisory")
				}
			})
		}
	})

	t.Run("a context that never passed through the middleware has no identity", func(t *testing.T) {
		// The failure this guards against is a handler treating "no principal"
		// as "some zero-valued principal" -- which, for an agent id, means the
		// empty string, and an empty string that compares equal to another
		// empty string is an authorization bypass waiting to happen.
		for _, tc := range []struct {
			name string
			ctx  context.Context
		}{
			{"nil", nil},
			{"background", context.Background()},
			{"a context carrying an unrelated value", context.WithValue(context.Background(), ctxKeyRequestID, "abc123")},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				if p, ok := PrincipalFromContext(tc.ctx); ok {
					t.Errorf("PrincipalFromContext returned ok with principal %+v", p)
				}
				if got := AgentIDFromContext(tc.ctx); got != "" {
					t.Errorf("AgentIDFromContext = %q, want the empty string", got)
				}
			})
		}
	})

	t.Run("the request id resolves inside the middleware", func(t *testing.T) {
		// LoggingMiddleware is wired OUTSIDE authMiddleware in New, and that
		// order is load-bearing: it is what puts the request id in the context
		// before authMiddleware runs, so a refusal line in the log can be
		// correlated with the response the client got. Flip the two and every
		// 401 becomes an uncorrelatable log entry.
		srv, logBuf := newMWServer(t)

		req := httptest.NewRequest(http.MethodGet, mwProbePath, nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		id := w.Result().Header.Get(RequestIDHeader)
		if id == "" {
			t.Fatalf("the 401 carries no %s header; a refused request must still be correlatable", RequestIDHeader)
		}

		// The refusal line comes from authMiddleware itself, so finding the
		// request id ON THAT LINE is the direct proof that the id was already in
		// the context when the middleware ran.
		var refusal string
		for _, line := range strings.Split(strings.TrimRight(logBuf.String(), "\n"), "\n") {
			if strings.Contains(line, "request refused") {
				refusal = line
				break
			}
		}
		if refusal == "" {
			t.Fatalf("no refusal line in the log, so this proves nothing; log was:\n%s", logBuf.String())
		}
		if !strings.Contains(refusal, "request_id="+id) {
			t.Errorf("the middleware's refusal line %q does not carry request_id=%s from the response; LoggingMiddleware must stay OUTSIDE authMiddleware", refusal, id)
		}

		// And on the way through: a handler behind the middleware sees the same
		// id, which is what a downstream 401-adjacent log line would use.
		rec := &mwRecorder{}
		token, _ := mwHandshake(t, srv, "alpha")
		req = httptest.NewRequest(http.MethodGet, mwProbePath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		LoggingMiddleware(srv.log, srv.authMiddleware(rec.handler())).ServeHTTP(w, req)

		if !rec.called {
			t.Fatalf("the handler was not reached; status %d", w.Code)
		}
		if rec.requestID == "" {
			t.Error("the handler saw no request id through the auth middleware")
		}
		if got := w.Result().Header.Get(RequestIDHeader); got != rec.requestID {
			t.Errorf("the handler saw request id %q but the response carries %q", rec.requestID, got)
		}
	})
}

// mwSyntheticPath is a PROTECTED route that exists only inside this test. It is
// deliberately NOT registered by New: a production route whose only purpose is
// to satisfy a test is worse than the gap it closes, so the route is built onto
// a private mux here instead.
const mwSyntheticPath = "/v1/synthetic"

// TestEveryRouteRequiresAuthOnASyntheticRoute exercises AUTH-6's ACTUAL CLAIM:
//
//	a route added later is authenticated because it was REGISTERED,
//	not because someone remembered to protect it.
//
// This is the only place that claim is under test today, and it exists because
// the enumeration loop in TestEveryRouteRequiresAuth ("every registered route
// off the allow-list refuses an anonymous caller") currently asserts NOTHING:
// all five registered routes are on the allow-list, so its `continue` fires for
// every one and the loop body never executes. A loop that has never run its
// body is not evidence that the loop works -- and that loop is precisely the
// thing meant to fail the day an unprotected route is added.
//
// So the property is pinned here instead, on a server that genuinely HAS a
// protected registered route. Nothing is faked: the stack is assembled from the
// real New, the real (*Server).route, the real authMiddleware and the real
// LoggingMiddleware, in the same order New wires them. The only thing this test
// adds is one more route -- exactly the event the property is about.
//
// When the first real protected route lands, the loop in authmw_test.go becomes
// live on its own and this test becomes a belt-and-braces duplicate of it. It
// should be kept anyway: it is the only version that also proves the NEGATIVE
// (the handler is never invoked on a refusal), which a status-code-only walk of
// the real surface cannot see.
//
// The name sits inside the proof_cmd regex `TestAuthMiddleware|TestEveryRouteRequiresAuth`
// on purpose -- -run matches unanchored, so `TestEveryRouteRequiresAuth` selects
// this test too and these assertions are part of the recorded proof rather than
// something that quietly never runs.
func TestEveryRouteRequiresAuthOnASyntheticRoute(t *testing.T) {
	// invocations counts every time the synthetic handler is entered. The
	// refusal assertions below are only worth something if this stays put: a
	// 401 body written AFTER the handler already ran would still look like a
	// 401 from outside.
	var invocations int
	var sawPrincipal auth.Principal
	var sawOK bool
	synthetic := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invocations++
		sawPrincipal, sawOK = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})

	// The handshake runs FIRST, against the server New actually built, because
	// the credential-issuing routes live on New's mux and the rebuild below
	// replaces it. The resulting token is minted by, and remains valid in, the
	// *auth.Service -- which the rebuild does not touch.
	srv, _ := newMWServer(t)
	token, agentID := mwHandshake(t, srv, "alpha")

	// Rebuild the stack by hand with one allow-listed route and one protected
	// route, mirroring New's own wiring (LoggingMiddleware OUTSIDE
	// authMiddleware, authMiddleware wrapping the WHOLE mux).
	mux := http.NewServeMux()
	srv.routes = nil
	srv.route(mux, "/healthz", srv.handleHealthz)
	srv.route(mux, mwSyntheticPath, synthetic)
	srv.handler = LoggingMiddleware(srv.log, srv.authMiddleware(mux))

	t.Run("the protected route refuses an anonymous caller and never runs the handler", func(t *testing.T) {
		invocations = 0

		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, mwSyntheticPath, nil))

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s with NO credential = %d, want 401; it is registered and is not on the allow-list, so the middleware must refuse it (invariant 3)", mwSyntheticPath, w.Code)
		}
		// The load-bearing half. Nobody had to protect this route: it was never
		// named in authmw.go, never added to an allow-list, never wrapped
		// individually. It is refused because authMiddleware wraps the whole mux
		// and is default-deny.
		if invocations != 0 {
			t.Fatalf("the synthetic handler ran %d time(s) on an unauthenticated request; a 401 must be terminal, not advisory", invocations)
		}
	})

	t.Run("the protected route is reachable with a valid token", func(t *testing.T) {
		// The counterweight to the refusal above: without this, a middleware
		// that refused EVERYTHING would pass the previous subtest.
		invocations = 0

		req := httptest.NewRequest(http.MethodGet, mwSyntheticPath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if w.Code != http.StatusNoContent {
			t.Fatalf("GET %s with a valid token = %d, want 204 from the synthetic handler", mwSyntheticPath, w.Code)
		}
		if invocations != 1 {
			t.Fatalf("the synthetic handler ran %d time(s) on an authenticated request, want exactly 1", invocations)
		}
		if !sawOK {
			t.Fatal("the handler on a protected route saw no principal; an authenticated route must be able to name its caller")
		}
		// Invariant 2: the identity handed to a handler is always
		// `<bus-id>.<agent-id>`, never a bare short name.
		if want := mwTestBusID + ".alpha-1"; sawPrincipal.AgentID != want {
			t.Errorf("principal.AgentID = %q, want the fully-qualified %q", sawPrincipal.AgentID, want)
		}
		if sawPrincipal.AgentID != agentID {
			t.Errorf("principal.AgentID = %q, but /v1/enroll minted %q; a handler must act on the id the server issued", sawPrincipal.AgentID, agentID)
		}
	})

	t.Run("the allow-listed route on the same mux is still anonymous", func(t *testing.T) {
		// Proves the filter's OTHER branch: default-deny did not simply swallow
		// the whole surface. /healthz is registered on this same mux and is
		// served without a credential.
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /healthz = %d, want 200; the allow-list must still short-circuit", w.Code)
		}
	})

	t.Run("the enumeration filter yields at least one protected route here", func(t *testing.T) {
		// THE POINT OF THIS TEST. This runs the exact filter
		// TestEveryRouteRequiresAuth runs -- walk Routes(), skip anything
		// IsUnauthenticatedRoute reports -- and shows that on a server with a
		// protected route the loop body DOES execute and DOES assert. That is
		// the logic which, on the real surface, is currently skipped for every
		// route.
		routes := srv.Routes()
		if len(routes) == 0 {
			t.Fatal("Routes() is empty, so the walk below would pass vacuously")
		}

		invocations = 0
		var probed int
		for _, pattern := range routes {
			if IsUnauthenticatedRoute(pattern) {
				continue
			}
			probed++
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, pattern, nil))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("GET %s with NO credential = %d, want 401", pattern, w.Code)
			}
		}

		if probed == 0 {
			t.Fatalf("the filter skipped every one of %v, so the loop body never ran and this test proves exactly what it was written to stop proving", routes)
		}
		if invocations != 0 {
			t.Errorf("the synthetic handler ran %d time(s) during the anonymous walk", invocations)
		}
	})
}
