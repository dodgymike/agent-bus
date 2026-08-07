package httpapi_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// authProbePath is the path every refusal case in TestAuthMiddleware is aimed
// at. It is registered NOWHERE and is NOT on the allow-list, which is exactly
// what makes it a useful probe: the status alone says WHO decided.
//
//	401 -> the middleware refused, before the mux was ever consulted.
//	404 -> the middleware passed the request through and the mux, honestly,
//	       has no route there.
//
// A probe aimed at a real route could not tell those apart, because a handler
// answering 200 and a middleware answering 200 look identical from outside.
const authProbePath = "/v1/messages/send"

// authForgedToken is syntactically perfect and was never issued by anything:
// 47 characters of the base64url alphabet, so it passes every one of
// bearerToken's syntactic rules and can only be rejected by
// auth.Service.Authenticate. It is deliberately a distinctive string so the
// "never logged" assertions below are searching for something that could
// actually be found if it leaked.
const authForgedToken = "ZmFrZS10b2tlbi1uZXZlci1pc3N1ZWQtYnktdGhpcy1idXM"

// authMWClock is a settable clock driving the auth service's expiry.
//
// Expiry is tested by MOVING THIS, never by time.Sleep: a sleep long enough to
// cross auth.SessionLifetime would take an hour, and a shortened lifetime would
// be testing a different constant than the one that ships.
type authMWClock struct {
	mu sync.Mutex
	t  time.Time
}

func newAuthMWClock() *authMWClock {
	// A fixed instant, not time.Now: nothing here depends on the wall clock.
	return &authMWClock{t: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)}
}

func (c *authMWClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *authMWClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newAuthMWServer builds a Server wired to a REAL *auth.Service on a settable
// clock, and returns the buffer capturing every log record at debug level.
//
// The service is real for the same reason newAuthServer's is: this is a test of
// the middleware's CONTRACT with internal/auth, and a stub would let the two
// drift apart with no test noticing. A stub that "authenticates" would in
// particular never reproduce the pending and expired cases, which are the two
// an implementation is most likely to get wrong.
func newAuthMWServer(t *testing.T) (*httpapi.Server, *bytes.Buffer, *authMWClock) {
	t.Helper()

	minter, err := ids.NewAgentIDMinter(authTestBusID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	clk := newAuthMWClock()
	svc, err := auth.NewService(auth.Options{Minter: minter, Now: clk.now})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}

	var logBuf bytes.Buffer
	srv := httpapi.New(httpapi.Options{
		Identity: testIdentity(authTestBusID),
		Logger:   logging.New(&logBuf, logging.LevelDebug),
		Auth:     svc,
	})
	return srv, &logBuf, clk
}

// authMWBeginSession runs POST /v1/session/begin and returns the PENDING token.
// A pending token is not a credential; see authMWHandshake.
func authMWBeginSession(t *testing.T, srv *httpapi.Server, agentID string) string {
	t.Helper()
	rec := postJSON(t, srv, httpapi.RouteSessionBegin, `{"agent_id":"`+agentID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("session/begin status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	token, _ := decodeBody(t, rec)["token"].(string)
	if token == "" {
		t.Fatalf("session/begin returned no token: %s", rec.Body.String())
	}
	return token
}

// authMWHandshake performs the FULL real handshake over HTTP, exactly as a
// scripts/bus-*.sh wrapper does it: enrol with a fresh Ed25519 keypair, ask for
// a challenge, sign auth.SessionSigningContext + token with the private half,
// complete. It returns an ACTIVE token and the server-minted agent id.
//
// Nothing here shortcuts into the auth service: if the handler and the service
// disagreed about any step, this helper would fail rather than hand back a
// token the middleware could not resolve.
func authMWHandshake(t *testing.T, srv *httpapi.Server, name, idemKey string) (token, agentID string) {
	t.Helper()

	_, priv, pubB64 := newAuthKeypair(t)
	agentID = enrolOverHTTP(t, srv, name, pubB64, idemKey)
	token = authMWBeginSession(t, srv, agentID)

	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(auth.SessionSigningContext+token)))
	rec := postJSON(t, srv, httpapi.RouteSessionComplete, `{"token":"`+token+`","signature":"`+sig+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("session/complete status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	return token, agentID
}

// authProbe issues GET path with the supplied Authorization header values, one
// Header.Add per value — so zero values means the header is absent entirely and
// two values means the request genuinely carries two of them.
func authProbe(t *testing.T, srv *httpapi.Server, path string, authValues ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, v := range authValues {
		req.Header.Add("Authorization", v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestAuthMiddleware covers AUTH-2: the 401 contract of the bearer-token
// middleware, and the positive case that proves a valid credential is let
// through.
func TestAuthMiddleware(t *testing.T) {
	t.Run("every unusable credential is 401", func(t *testing.T) {
		// values chooses the Authorization header(s) for a case. token is a
		// freshly-completed, currently-VALID session token on the same server, so
		// a case can build a header that is malformed in exactly one way while
		// the credential inside it is genuine.
		cases := []struct {
			name   string
			values func(t *testing.T, srv *httpapi.Server, clk *authMWClock, token, agentID string) []string
			// wantChallenge is the RFC 6750 error code the WWW-Authenticate
			// header must carry: invalid_request for a credential that was
			// absent or not well-formed, invalid_token for one that was
			// well-formed and did not authenticate.
			wantChallenge string
			because       string
		}{
			{
				name:          "no Authorization header at all",
				values:        func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string { return nil },
				wantChallenge: `error="invalid_request"`,
				because:       "nothing was presented, so there is nothing to call invalid",
			},
			{
				name: "Bearer with nothing after it",
				values: func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string {
					return []string{"Bearer"}
				},
				wantChallenge: `error="invalid_request"`,
				because:       "a scheme with no credential is not a well-formed Authorization header",
			},
			{
				name: "Bearer with only spaces after it",
				values: func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string {
					return []string{"Bearer   "}
				},
				wantChallenge: `error="invalid_request"`,
				because:       "whitespace is not a token, and must not be trimmed into an empty one",
			},
			{
				name: "the wrong scheme entirely",
				values: func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string {
					return []string{"Basic dXNlcjpwYXNz"}
				},
				wantChallenge: `error="invalid_request"`,
				because:       "this server accepts exactly one scheme",
			},
			{
				name: "a valid token with no scheme at all",
				values: func(_ *testing.T, _ *httpapi.Server, _ *authMWClock, token, _ string) []string {
					return []string{token}
				},
				wantChallenge: `error="invalid_request"`,
				because:       "the credential is real but the header is not; a bare token must not be accepted",
			},
			{
				name: "two Authorization headers carrying the SAME VALID token",
				values: func(_ *testing.T, _ *httpapi.Server, _ *authMWClock, token, _ string) []string {
					return []string{"Bearer " + token, "Bearer " + token}
				},
				wantChallenge: `error="invalid_request"`,
				// This is the case that matters. Both values authenticate, so a
				// 401 here can ONLY come from the duplicate being rejected on
				// ambiguity -- not incidentally because the value was bad. A
				// proxy in front of this server could have produced the second
				// header, and choosing which of two credentials to honour is
				// precisely how the front and back halves of a stack are made to
				// disagree about who the caller is.
				because: "duplicate credentials are refused on ambiguity, never resolved by picking one",
			},
			{
				name: "a token containing a space",
				values: func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string {
					return []string{"Bearer abc def"}
				},
				wantChallenge: `error="invalid_request"`,
				because:       "the split is on the FIRST space, and what follows must be one token",
			},
			{
				name: "a token one byte over MaxBearerTokenLen",
				values: func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string {
					return []string{"Bearer " + strings.Repeat("a", httpapi.MaxBearerTokenLen+1)}
				},
				wantChallenge: `error="invalid_request"`,
				because:       "an unauthenticated caller must not push an unbounded string into the hashing path",
			},
			{
				name: "a token containing +",
				values: func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string {
					return []string{"Bearer abc+def"}
				},
				wantChallenge: `error="invalid_request"`,
				because:       "+ is standard base64, not base64url; internal/auth mints RawURLEncoding",
			},
			{
				name: "a token containing /",
				values: func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string {
					return []string{"Bearer abc/def"}
				},
				wantChallenge: `error="invalid_request"`,
				because:       "same as + -- outside the alphabet this server ever issues",
			},
			{
				name: "a token containing = padding",
				values: func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string {
					return []string{"Bearer abcdef=="}
				},
				wantChallenge: `error="invalid_request"`,
				because:       "RawURLEncoding is unpadded; padding means this did not come from here",
			},
			{
				name: "a token containing a dot",
				values: func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string {
					return []string{"Bearer abc.def.ghi"}
				},
				wantChallenge: `error="invalid_request"`,
				because:       "a JWT-shaped string is not a credential here; tokens are opaque handles",
			},
			{
				name: "a token containing a non-ASCII byte",
				values: func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string {
					return []string{"Bearer abc\xc3\xa9def"}
				},
				wantChallenge: `error="invalid_request"`,
				because:       "the syntactic filter is an allow-list of bytes, not a deny-list",
			},
			{
				name: "a well-formed token that was never issued",
				values: func(*testing.T, *httpapi.Server, *authMWClock, string, string) []string {
					return []string{"Bearer " + authForgedToken}
				},
				wantChallenge: `error="invalid_token"`,
				because:       "it passes every syntactic rule, so only Authenticate can refuse it",
			},
			{
				name: "a PENDING token whose challenge was never completed",
				values: func(t *testing.T, srv *httpapi.Server, _ *authMWClock, _, agentID string) []string {
					// /v1/session/begin issues this token and it is byte-for-byte
					// the shape of a live one. It is NOT a credential until the
					// signature verifies, and this is the case an implementation
					// that merely looked the token up in the session table would
					// get wrong.
					return []string{"Bearer " + authMWBeginSession(t, srv, agentID)}
				},
				wantChallenge: `error="invalid_token"`,
				because:       "an unsigned challenge is not a credential",
			},
			{
				name: "an EXPIRED token",
				values: func(_ *testing.T, _ *httpapi.Server, clk *authMWClock, token, _ string) []string {
					// Server-side expiry with no skew grace: one nanosecond past
					// the deadline is over.
					clk.advance(auth.SessionLifetime + time.Nanosecond)
					return []string{"Bearer " + token}
				},
				wantChallenge: `error="invalid_token"`,
				because:       "the token is still syntactically perfect; only the clock changed",
			},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				// A fresh server per case so a case that advances the clock or
				// burns a challenge cannot perturb another.
				srv, _, clk := newAuthMWServer(t)
				token, agentID := authMWHandshake(t, srv, "alpha", "idem-1")

				rec := authProbe(t, srv, authProbePath, tc.values(t, srv, clk, token, agentID)...)

				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("GET %s status = %d, want 401 (%s); body %s", authProbePath, rec.Code, tc.because, rec.Body.String())
				}
				if got := rec.Result().Header.Get("WWW-Authenticate"); !strings.Contains(got, tc.wantChallenge) {
					t.Errorf("WWW-Authenticate = %q, want it to carry %s", got, tc.wantChallenge)
				}
				if got := rec.Result().Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
					t.Errorf("Content-Type = %q, want %q: a client parses ONE error format", got, "application/json; charset=utf-8")
				}
				wantKeys(t, decodeBody(t, rec), "error")
			})
		}
	})

	t.Run("a valid token reaches the mux", func(t *testing.T) {
		srv, _, _ := newAuthMWServer(t)
		token, _ := authMWHandshake(t, srv, "alpha", "idem-1")

		// 404 IS THE PASS, and 401 is the failure this asserts against.
		//
		// authProbePath is registered nowhere, so the only component that can
		// answer 404 is the mux -- which means the middleware accepted the
		// credential and handed the request onward. If the middleware had
		// refused, the caller would never have reached the mux and would see
		// 401. Asserting 200 instead would require a protected route to exist,
		// and AUTH-2 deliberately adds none.
		rec := authProbe(t, srv, authProbePath, "Bearer "+token)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s with a valid token = %d, want 404: the middleware must pass the request to the mux, which has no such route; body %s", authProbePath, rec.Code, rec.Body.String())
		}
	})

	t.Run("the Bearer scheme is case-insensitive", func(t *testing.T) {
		// RFC 7235: the auth-scheme is case-insensitive. A client library that
		// spells it "bearer" is conforming, and refusing it would be a bug that
		// looks like an authentication failure.
		for _, scheme := range []string{"bearer", "BeArEr", "BEARER", "Bearer"} {
			scheme := scheme
			t.Run(scheme, func(t *testing.T) {
				srv, _, _ := newAuthMWServer(t)
				token, _ := authMWHandshake(t, srv, "alpha", "idem-1")

				rec := authProbe(t, srv, authProbePath, scheme+" "+token)
				if rec.Code != http.StatusNotFound {
					t.Fatalf("GET %s with %q = %d, want 404 (reached the mux); 401 would mean the scheme spelling was rejected", authProbePath, scheme, rec.Code)
				}
			})
		}
	})

	t.Run("an allow-listed route is unaffected by a credential", func(t *testing.T) {
		srv, _, _ := newAuthMWServer(t)
		token, _ := authMWHandshake(t, srv, "alpha", "idem-1")

		cases := []struct {
			name       string
			authValues []string
		}{
			{"without any credential", nil},
			{"with a valid credential", []string{"Bearer " + token}},
			{"with a forged credential", []string{"Bearer " + authForgedToken}},
			{"with a malformed header", []string{"Basic dXNlcjpwYXNz"}},
		}

		for _, path := range []string{"/healthz", "/v1/info"} {
			for _, tc := range cases {
				tc := tc
				path := path
				t.Run(path+" "+tc.name, func(t *testing.T) {
					// Carrying a credential to an unauthenticated route must
					// change NOTHING: the allow-list short-circuits before the
					// header is looked at, so a bad one cannot turn a 200 into a
					// 401 and a good one cannot be required by accident.
					rec := authProbe(t, srv, path, tc.authValues...)
					if rec.Code != http.StatusOK {
						t.Fatalf("GET %s %s = %d, want 200; body %s", path, tc.name, rec.Code, rec.Body.String())
					}
				})
			}
		}
	})

	t.Run("unknown pending and expired are byte-identical to the client", func(t *testing.T) {
		// This is a SECURITY property, not tidiness. If the three answers
		// differed, a caller holding one token could learn whether some other
		// token exists, whether it is merely unsigned, or whether it once was
		// valid -- an enumeration oracle assembled entirely out of "helpful"
		// error messages. The log is where the difference belongs; the wire is
		// not.
		srv, _, clk := newAuthMWServer(t)
		token, agentID := authMWHandshake(t, srv, "alpha", "idem-1")
		pending := authMWBeginSession(t, srv, agentID)

		unknownRec := authProbe(t, srv, authProbePath, "Bearer "+authForgedToken)
		pendingRec := authProbe(t, srv, authProbePath, "Bearer "+pending)

		clk.advance(auth.SessionLifetime + time.Nanosecond)
		expiredRec := authProbe(t, srv, authProbePath, "Bearer "+token)

		type answer struct {
			name string
			rec  *httptest.ResponseRecorder
		}
		answers := []answer{
			{"unknown", unknownRec},
			{"pending", pendingRec},
			{"expired", expiredRec},
		}
		for _, a := range answers {
			if a.rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s token status = %d, want 401; body %s", a.name, a.rec.Code, a.rec.Body.String())
			}
		}

		base := answers[0]
		for _, a := range answers[1:] {
			if got, want := a.rec.Body.String(), base.rec.Body.String(); got != want {
				t.Errorf("the %s-token body is %q but the %s-token body is %q; they must be byte-identical, or the difference is an enumeration oracle. If you made one message more specific on purpose, that is a security regression, not a test to update", a.name, got, base.name, want)
			}
			if got, want := a.rec.Result().Header.Get("WWW-Authenticate"), base.rec.Result().Header.Get("WWW-Authenticate"); got != want {
				t.Errorf("the %s-token challenge is %q but the %s-token challenge is %q; they must be identical", a.name, got, base.name, want)
			}
		}

		// Guard against the assertion above passing because all three bodies are
		// empty or some unrelated shape.
		if got := base.rec.Body.String(); !strings.Contains(got, `"error"`) {
			t.Fatalf("the 401 body is %q, want the standard {\"error\":...} envelope, so this comparison is comparing the real thing", got)
		}
	})

	t.Run("the token never reaches the log the body or a header", func(t *testing.T) {
		srv, logBuf, clk := newAuthMWServer(t)
		token, agentID := authMWHandshake(t, srv, "alpha", "idem-1")

		// Three shapes of request, because the easiest place for an untrusted
		// value to be written "for diagnosis" is an error path.
		recs := map[string]*httptest.ResponseRecorder{
			"authenticated": authProbe(t, srv, authProbePath, "Bearer "+token),
			"refused":       authProbe(t, srv, authProbePath, "Bearer "+authForgedToken),
		}
		clk.advance(auth.SessionLifetime + time.Nanosecond)
		recs["expired"] = authProbe(t, srv, authProbePath, "Bearer "+token)

		if got := recs["authenticated"].Code; got != http.StatusNotFound {
			t.Fatalf("the authenticated probe returned %d, want 404; this test must exercise the success path too", got)
		}

		logs := logBuf.String()

		// Guard against a vacuous pass: the buffer must actually hold the
		// records these requests produced.
		if len(logLines(logBuf)) == 0 {
			t.Fatal("no log records captured, so this test proves nothing")
		}
		if !strings.Contains(logs, agentID) {
			t.Fatalf("the log never mentions %q, so it is not the log for these requests and the absence of the token proves nothing", agentID)
		}

		secrets := []struct {
			what  string
			value string
		}{
			{"the live session token", token},
			{"the forged token", authForgedToken},
		}
		for _, s := range secrets {
			if strings.Contains(logs, s.value) {
				t.Errorf("%s reached the server log; log retention must never become credential retention", s.what)
			}
			// A truncated or hashed token is still a token-derived value in a
			// log line, and the middleware's contract is that NOTHING derived
			// from it leaves. The 16-character prefix is the shape a
			// "just enough to correlate" change would take.
			if len(s.value) >= 16 && strings.Contains(logs, s.value[:16]) {
				t.Errorf("a prefix of %s reached the server log", s.what)
			}
			for name, rec := range recs {
				if strings.Contains(rec.Body.String(), s.value) {
					t.Errorf("%s was echoed into the %s response body", s.what, name)
				}
				for header, values := range rec.Result().Header {
					for _, v := range values {
						if strings.Contains(v, s.value) {
							t.Errorf("%s was echoed into the %s response header %s", s.what, name, header)
						}
					}
				}
			}
		}
	})

	t.Run("a server built without an auth service serves the allow-list and nothing else", func(t *testing.T) {
		// Nobody can be authenticated here, and the middleware is default-deny,
		// so every path off the allow-list is 401. It must not panic on the nil
		// service, and it must not accidentally fail OPEN.
		var logBuf bytes.Buffer
		srv := httpapi.New(httpapi.Options{
			Identity: testIdentity(authTestBusID),
			Logger:   logging.New(&logBuf, logging.LevelDebug),
		})
		if srv.Auth() != nil {
			t.Fatalf("Auth() = %v, want nil", srv.Auth())
		}

		for _, values := range [][]string{nil, {"Bearer " + authForgedToken}} {
			rec := authProbe(t, srv, authProbePath, values...)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("GET %s with Authorization %v = %d, want 401 when there is no auth service", authProbePath, values, rec.Code)
			}
			wantKeys(t, decodeBody(t, rec), "error")
		}

		for _, path := range []string{"/healthz", "/v1/info"} {
			if rec := authProbe(t, srv, path); rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200: the allow-list is still served without an auth service", path, rec.Code)
			}
		}
	})
}

// TestEveryRouteRequiresAuth is the AUTH-6 deliverable: the test whose entire
// job is to FAIL the day someone registers a route and forgets to think about
// authentication.
//
// It walks the server's REAL registered surface rather than a hand-maintained
// list, so a new route is covered by being registered, not by someone
// remembering to add it here.
func TestEveryRouteRequiresAuth(t *testing.T) {
	srv, _, _ := newAuthMWServer(t)
	routes := srv.Routes()

	// MANDATORY GUARD, and not a formality: a range over an empty slice passes
	// without executing a single assertion. If Routes() ever returned nothing --
	// a refactor that stops recording patterns, a build that registers none --
	// every check below would report success having proved nothing at all.
	if len(routes) == 0 {
		t.Fatal("Routes() is empty, so the enumeration below would pass vacuously; every route must be registered through (*Server).route")
	}

	t.Run("every registered route off the allow-list refuses an anonymous caller", func(t *testing.T) {
		// probed counts the routes this loop actually ASSERTED on, i.e. the ones
		// the allow-list did not skip. The guard above (len(routes) == 0) does
		// not catch the shape that matters here: the slice is non-empty, it is
		// the FILTERED set that can be empty, and a range whose body is skipped
		// every iteration reports success having checked nothing.
		var probed int

		for _, pattern := range routes {
			pattern := pattern
			if httpapi.IsUnauthenticatedRoute(pattern) {
				continue
			}
			probed++
			t.Run(pattern, func(t *testing.T) {
				rec := authProbe(t, srv, pattern)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("GET %s with NO credential = %d, want 401.\n"+
						"This route is registered and is not on httpapi.UnauthenticatedRoutes(), so it must refuse an anonymous caller (invariant 3).\n"+
						"Do ONE of these:\n"+
						"  - leave it protected (nothing to do -- the middleware is default-deny, so a 401 here is the normal outcome and something has bypassed it), or\n"+
						"  - if it genuinely must be public, add it to unauthenticatedRoutes in internal/httpapi/authmw.go AND to the golden list in this test, with a comment saying why. That edit is meant to be visible in review.",
						pattern, rec.Code)
				}
			})
		}

		// NOT a failure, on purpose. Zero protected routes is the honest state
		// of this build: every route registered so far -- /healthz, /v1/info and
		// the three credential-issuing routes -- is legitimately on the
		// allow-list, and failing here would only pressure someone into
		// registering a fake route to silence it, which is worse than the gap.
		//
		// It is logged loudly because the number that matters is invisible
		// otherwise: `go test -v` shows this subtest PASSing with no children,
		// which is indistinguishable from a subtest that checked everything.
		if probed == 0 {
			t.Logf("NOTE: this loop asserted NOTHING on this build.\n"+
				"  All %d registered routes (%v) are on the allow-list, so the filter skipped every one and the loop body never executed.\n"+
				"  The property it is meant to enforce -- a route added later is authenticated because it was REGISTERED, not because someone remembered to protect it -- is pinned meanwhile by\n"+
				"      TestEveryRouteRequiresAuthOnASyntheticRoute (internal/httpapi/authmw_internal_test.go),\n"+
				"  which stands the real middleware in front of a genuinely protected route and runs this same filter over it.\n"+
				"  The FIRST real protected route makes this loop live, and this note disappears.",
				len(routes), routes)
		}
	})

	t.Run("the allow-list is exactly these six paths", func(t *testing.T) {
		// A GOLDEN list, on purpose. Every entry here is a path this server
		// serves to an anonymous caller, so ADDING one must show up as a
		// deliberate diff in a test file called "every route requires auth" --
		// which is precisely the review moment this test exists to force. It is
		// not a duplicate of the map in authmw.go; it is the second signature
		// required to change it.
		//
		// DISCOVERY-DOC added exactly one entry: /v1/discovery. It earns its
		// place because it is how a caller holding NOTHING BUT A URL learns how
		// to enrol -- requiring a credential to learn how to obtain a credential
		// is a circular gate, which is the same reason /v1/enroll and the two
		// session routes are here. It is safe because the document it serves is
		// a STATIC COMPILE-TIME CONSTANT plus this bus's id (which /v1/info
		// already serves to the same anonymous caller): it describes the
		// PROTOCOL, never the ROSTER, and reveals nothing about this bus's
		// contents or its configuration -- notably NOT which routes this build
		// registered, which is what the 401-not-404 choice below exists to
		// withhold. That property is pinned exhaustively by
		// TestDiscoveryDocumentIsStatic and TestDiscoveryDocumentLeaksNoBusState
		// in discovery_test.go; if either of those goes slack, this entry stops
		// being justified.
		want := []string{
			"/healthz",             // liveness; returns no state, called before any agent exists
			"/v1/discovery",        // the protocol-discovery document: static, bus-state-free, and the only way a caller with just a URL learns to enrol
			"/v1/enroll",           // creates an identity; there is no credential yet by definition
			"/v1/info",             // pre-enrolment discovery: bus id, version, uptime
			"/v1/session/begin",    // asks for the token to sign; called with no session at all
			"/v1/session/complete", // authenticated by the Ed25519 signature, not by a bearer token
		}
		got := httpapi.UnauthenticatedRoutes()

		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("UnauthenticatedRoutes() = %v, want exactly %v (sorted).\n"+
				"If you ADDED an entry: that is a new anonymous surface. Justify it in the comment above and in internal/httpapi/authmw.go.\n"+
				"If you REMOVED one: check nothing in the credential-issuing handshake now requires a credential it cannot have.",
				got, want)
		}
	})

	t.Run("every allow-list entry is a route that actually exists", func(t *testing.T) {
		// A stale or misspelled entry protects nothing and, worse, reads like it
		// documents something real. The server under test was built WITH an auth
		// service, which is the only build where the three credential routes are
		// registered at all.
		registered := make(map[string]bool, len(routes))
		for _, pattern := range routes {
			registered[pattern] = true
		}
		for _, path := range httpapi.UnauthenticatedRoutes() {
			if !registered[path] {
				t.Errorf("%q is on the allow-list but is not a registered route (registered: %v); either it is a typo or it names a route that no longer exists", path, routes)
			}
		}
	})

	t.Run("a path that is registered nowhere is refused not 404ed", func(t *testing.T) {
		// DEFAULT-DENY. An anonymous caller must not be able to map this bus's
		// surface by reading status codes: an unknown path and a known one look
		// the same until a credential is presented.
		//
		// The near-miss spellings are the point of the exercise. Matching is
		// EXACT string equality on r.URL.Path -- no cleaning, no trailing-slash
		// tolerance, no case folding -- so every one of these misses the
		// allow-list and is refused. A lenient match here, disagreeing with what
		// the mux does with the same string, is exactly how an allow-list bypass
		// is built.
		cases := []struct {
			name string
			path string
		}{
			{"a future route", "/v1/messages/send"},
			{"nonsense", "/nope"},
			{"a future relay route", "/v1/relay/peer"},
			{"a doubled leading slash", "//healthz"},
			{"a trailing slash on healthz", "/healthz/"},
			{"a trailing slash on info", "/v1/info/"},
			{"healthz in upper case", "/HEALTHZ"},
			{"a dot-dot walk back to an allow-listed path", "/v1/../healthz"},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec := authProbe(t, srv, tc.path)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("GET %s with no credential = %d, want 401; %q must not reach the mux unauthenticated", tc.path, rec.Code, tc.path)
				}
			})
		}
	})

	t.Run("IsUnauthenticatedRoute agrees with UnauthenticatedRoutes", func(t *testing.T) {
		for _, path := range httpapi.UnauthenticatedRoutes() {
			if !httpapi.IsUnauthenticatedRoute(path) {
				t.Errorf("IsUnauthenticatedRoute(%q) = false, but %q is on the list the same package returns", path, path)
			}
		}
		for _, path := range []string{
			"//healthz", "/healthz/", "/HEALTHZ", "/v1/info/", "/v1/Info",
			"/v1/enroll/", "/v1/session/begin/", "/v1/../healthz", "healthz", "",
		} {
			if httpapi.IsUnauthenticatedRoute(path) {
				t.Errorf("IsUnauthenticatedRoute(%q) = true; matching is exact, and a non-canonical spelling must require a credential", path)
			}
		}
	})

	t.Run("the allow-list and the route list are returned as copies", func(t *testing.T) {
		// The allow-list is this server's security boundary. A caller that got a
		// handle on the real one could open a route to anonymous callers from
		// anywhere in the process, with no diff in internal/httpapi to review.
		first := httpapi.UnauthenticatedRoutes()
		if len(first) == 0 {
			t.Fatal("UnauthenticatedRoutes() is empty, so mutating it proves nothing")
		}
		first[0] = "/pwned"
		first = append(first, "/also-pwned")
		if second := httpapi.UnauthenticatedRoutes(); second[0] == "/pwned" || len(second) != len(first)-1 {
			t.Fatalf("UnauthenticatedRoutes() = %v after a caller mutated a previous return value; it must hand back a copy", second)
		}
		if httpapi.IsUnauthenticatedRoute("/pwned") {
			t.Error("mutating the returned slice changed the actual allow-list")
		}

		firstRoutes := srv.Routes()
		if len(firstRoutes) == 0 {
			t.Fatal("Routes() is empty, so mutating it proves nothing")
		}
		firstRoutes[0] = "/pwned"
		if second := srv.Routes(); second[0] == "/pwned" {
			t.Fatalf("Routes() = %v after a caller mutated a previous return value; it must hand back a copy", second)
		}
	})
}
