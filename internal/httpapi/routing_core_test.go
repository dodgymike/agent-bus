package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/httpapi"
)

// Every expectation in this file is written as a LITERAL rather than as the
// package constant that produces it (httpapi.AllowGET, httpapi.RouteCatchAll).
// That is deliberate: a test that reads the constant it is checking asserts
// only that the constant equals itself, and would go green if someone changed
// the header value or the pattern. These are the strings on the wire, so the
// test spells them out. It also means every case below COMPILES against the
// pre-fix code, which is what let each one be observed RED first.

// getRoutesUnderTest lists every route guarded by requireGET, with what a
// caller needs to reach it. `authenticate` says whether the route is behind
// the default-deny middleware.
type getRouteCase struct {
	name         string
	path         string
	authenticate bool
}

var getRoutesUnderTest = []getRouteCase{
	{"healthz", "/healthz", false},
	{"info", "/v1/info", false},
	{"discovery", httpapi.RouteDiscovery, false},
	{"agents", httpapi.RouteAgents, true},
	{"messages", httpapi.RouteMessages, true},
	// timeout=1 is the smallest value readTimeoutParam accepts (whole
	// seconds), so this parks for a second rather than for the server
	// default. It is included anyway: /v1/wait is a GET route, and CORE-7
	// asks for the status of HEAD on EVERY GET route, not on the convenient
	// ones.
	{"wait", httpapi.RouteWait + "?timeout=1", true},
}

// TestHeadRequest pins CORE-7: HEAD is ACCEPTED on every GET route, answers
// the same status as GET, and carries no body.
//
// Before the fix requireGET matched only http.MethodGet, so every case here
// answered 405 -- including HEAD /healthz, which load balancers, container
// healthchecks and uptime probes all issue by default. Meanwhile writeJSON
// carried an `if r.Method != http.MethodHead` body-suppression guard that
// could never be reached, so the two halves of the package stated opposite
// intentions. This test is what makes the guard live and pins which of the two
// answers the server actually gives.
func TestHeadRequest(t *testing.T) {
	srv, _ := newMessagingServer(t)
	caller := enrolAndAuthenticate(t, srv, "head-prober")

	do := func(t *testing.T, method string, c getRouteCase) *httptest.ResponseRecorder {
		t.Helper()
		if c.authenticate {
			return authed(t, srv, caller, method, c.path, "")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(method, c.path, nil))
		return rec
	}

	// MANDATORY GUARD: a range over an empty table passes having asserted
	// nothing, and this table is hand-maintained.
	if len(getRoutesUnderTest) == 0 {
		t.Fatal("getRoutesUnderTest is empty, so every loop below would pass vacuously")
	}

	t.Run("HEAD answers the same status as GET, with no body", func(t *testing.T) {
		for _, c := range getRoutesUnderTest {
			c := c
			t.Run(c.name, func(t *testing.T) {
				getRec := do(t, http.MethodGet, c)
				headRec := do(t, http.MethodHead, c)

				if headRec.Code != getRec.Code {
					t.Fatalf("HEAD %s = %d, want %d (the same status GET gave).\n"+
						"CORE-7: HEAD is accepted on GET routes. A 405 here means requireGET stopped matching http.MethodHead.\n"+
						"body=%q", c.path, headRec.Code, getRec.Code, headRec.Body.String())
				}
				if headRec.Code != http.StatusOK {
					t.Fatalf("GET %s = %d, want 200 -- the fixture is wrong, so the HEAD comparison above proved nothing", c.path, headRec.Code)
				}
				// httptest.ResponseRecorder does NOT suppress bodies for HEAD
				// (that is the real net/http server's job), so an empty body
				// here is evidence that writeJSON's own MethodHead guard ran.
				// That guard is precisely the dead code CORE-7 named.
				if headRec.Body.Len() != 0 {
					t.Fatalf("HEAD %s wrote a %d-byte body: %q.\n"+
						"writeJSON must suppress the body for HEAD; RFC 9110 makes HEAD a GET without one.",
						c.path, headRec.Body.Len(), headRec.Body.String())
				}
				// Headers must otherwise match GET: a HEAD whose headers differ
				// from the GET it describes defeats the point of the method.
				if got, want := headRec.Header().Get("Content-Type"), getRec.Header().Get("Content-Type"); got != want {
					t.Fatalf("HEAD %s Content-Type = %q, want %q (same as GET)", c.path, got, want)
				}
			})
		}
	})

	t.Run("a genuinely unsupported method is still 405 and Allow names HEAD", func(t *testing.T) {
		for _, c := range getRoutesUnderTest {
			c := c
			t.Run(c.name, func(t *testing.T) {
				rec := do(t, http.MethodPut, c)
				if rec.Code != http.StatusMethodNotAllowed {
					t.Fatalf("PUT %s = %d, want 405 -- accepting HEAD must not have widened the method set", c.path, rec.Code)
				}
				// An Allow header that omitted a method the route serves would
				// be the same inconsistency CORE-7 fixed, one layer out.
				if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
					t.Fatalf("PUT %s Allow = %q, want %q; Allow must list every method the route serves", c.path, got, "GET, HEAD")
				}
			})
		}
	})

	t.Run("HEAD on a POST-only route is still 405, with a body-free response", func(t *testing.T) {
		// The other side of the decision: accepting HEAD on GET routes must not
		// make it acceptable everywhere. requirePOST is untouched -- and this
		// also exercises writeJSON's HEAD guard on an ERROR response, where a
		// body would be doubly wrong.
		rec := authed(t, srv, caller, http.MethodHead, httpapi.RouteSend, "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("HEAD %s = %d, want 405", httpapi.RouteSend, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodPost {
			t.Fatalf("HEAD %s Allow = %q, want %q", httpapi.RouteSend, got, http.MethodPost)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("HEAD %s wrote a body on its 405: %q", httpapi.RouteSend, rec.Body.String())
		}
	})

	t.Run("HEAD to a protected route without a credential is 401, not 200", func(t *testing.T) {
		// The security half. HEAD must go through default-deny exactly like
		// GET; a method that skipped the middleware would be a bypass.
		for _, c := range getRoutesUnderTest {
			if !c.authenticate {
				continue
			}
			c := c
			t.Run(c.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, c.path, nil))
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("anonymous HEAD %s = %d, want 401; HEAD must not bypass authMiddleware", c.path, rec.Code)
				}
			})
		}
	})
}

// TestNotFoundJSON pins CORE-8: an unmatched path answers the SAME JSON error
// envelope as every other failure, and does so WITHOUT becoming a route
// oracle.
//
// Before the fix the request fell through to net/http.ServeMux's built-in
// handler and got "404 page not found" as text/plain -- so a client, or a
// wrapper piping through a JSON parser, hit a parse error exactly when
// something was already wrong.
//
// The security constraint is the interesting half and is asserted here rather
// than assumed: the catch-all is registered INSIDE authMiddleware, so an
// anonymous caller still gets 401 on every path, known or not, and cannot read
// status codes to learn which surfaces this build serves.
func TestNotFoundJSON(t *testing.T) {
	srv, _, _ := newAuthMWServer(t)
	token, _ := authMWHandshake(t, srv, "notfound-prober", "idem-notfound-1")

	authedReq := func(t *testing.T, method, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	unknownPaths := []string{
		"/nope",
		"/v1/nope",
		"/v1/info/nope", // an unknown path UNDER a known one
		"/healthz/nope", // ditto, under the shortest known one
		"/v1/",          // a bare prefix of the real routes
		"/",             // the catch-all's own pattern
		"/v1/send",      // registered only when a hub is wired; this build has none
	}

	t.Run("an authenticated caller gets a JSON 404, not text/plain", func(t *testing.T) {
		for _, p := range unknownPaths {
			p := p
			t.Run(p, func(t *testing.T) {
				rec := authedReq(t, http.MethodGet, p)
				if rec.Code != http.StatusNotFound {
					t.Fatalf("GET %s with a valid credential = %d, want 404 (body=%q)", p, rec.Code, rec.Body.String())
				}
				ct := rec.Header().Get("Content-Type")
				if !strings.HasPrefix(ct, "application/json") {
					t.Fatalf("GET %s Content-Type = %q, want application/json.\n"+
						"This is the CORE-8 defect: net/http's built-in 404 is text/plain, so a client that trusts the documented JSON error contract gets a parse error instead of a structured one.",
						p, ct)
				}
				if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
					t.Fatalf("GET %s X-Content-Type-Options = %q, want nosniff (every other error carries it)", p, got)
				}

				var body httpapi.ErrorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("GET %s body is not the standard error envelope: %v (body=%q)", p, err, rec.Body.String())
				}
				if body.Error == "" {
					t.Fatalf("GET %s returned an empty error string: %q", p, rec.Body.String())
				}
				// The body must not reflect the requested path back. It is
				// attacker-controlled, it tells the caller nothing it did not
				// type, and this route has had no other validation applied.
				if strings.Contains(rec.Body.String(), "nope") {
					t.Fatalf("GET %s echoed the requested path into the response body: %q", p, rec.Body.String())
				}
			})
		}
	})

	t.Run("every method on an unknown path is 404, never 405", func(t *testing.T) {
		// 405 would assert the resource EXISTS but not via that verb -- false
		// here, and a disclosure: method-probing would separate "path exists,
		// wrong method" from "path does not exist".
		for _, m := range []string{
			http.MethodGet, http.MethodHead, http.MethodPost,
			http.MethodPut, http.MethodDelete, http.MethodPatch,
		} {
			m := m
			t.Run(m, func(t *testing.T) {
				rec := authedReq(t, m, "/definitely-not-a-route")
				if rec.Code != http.StatusNotFound {
					t.Fatalf("%s /definitely-not-a-route = %d, want 404 (body=%q)", m, rec.Code, rec.Body.String())
				}
			})
		}
	})

	t.Run("an unknown METHOD on a KNOWN path is a JSON 405", func(t *testing.T) {
		// CORE-8 asks for both halves. This one already worked; it is pinned so
		// the catch-all cannot start swallowing known paths.
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/healthz", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE /healthz = %d, want 405; the catch-all must not shadow a registered route", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("DELETE /healthz Content-Type = %q, want application/json", ct)
		}
	})

	t.Run("the catch-all is NOT an anonymous route: unknown paths stay 401", func(t *testing.T) {
		// THE SECURITY ASSERTION. A catch-all registered outside authMiddleware
		// would answer 404 here, and an anonymous caller could then tell a
		// served path (401) from an unserved one (404) and enumerate the whole
		// surface. Registered inside it, default-deny answers first and the two
		// are indistinguishable.
		if httpapi.IsUnauthenticatedRoute("/") {
			t.Fatal(`"/" is on the unauthenticated allow-list; the catch-all must never be, or it is a route oracle`)
		}
		for _, p := range unknownPaths {
			p := p
			t.Run(p, func(t *testing.T) {
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("anonymous GET %s = %d, want 401.\n"+
						"An anonymous caller must not be able to distinguish a served path from an unserved one.",
						p, rec.Code)
				}
			})
		}

		// And the comparison that makes the above mean something: a KNOWN
		// protected path answers anonymously with the identical status.
		known := httptest.NewRecorder()
		srv.ServeHTTP(known, httptest.NewRequest(http.MethodGet, "/v1/agents", nil))
		unknown := httptest.NewRecorder()
		srv.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/v1/agents-not-a-route", nil))
		if known.Code != unknown.Code {
			t.Fatalf("anonymous GET of a known protected path = %d but of an unknown path = %d; the difference IS the oracle",
				known.Code, unknown.Code)
		}
	})
}
