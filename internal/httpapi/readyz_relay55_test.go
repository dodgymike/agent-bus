package httpapi

// RELAY-55: GET /v1/readyz — federation readiness.
//
// A bus can be healthy on /healthz (the process is up) yet silently deaf to the
// entire federation because its peer ingress surface never mounted. /healthz
// cannot see that; /v1/readyz is the authenticated signal that can.
//
// These are WHITE-BOX tests because the state the route reports on —
// s.peerRoutes (did the surface mount) and s.federationExpected (were peer
// records present) — is unexported, and the "ready" case needs the real mount
// that only New performs when a complete PeerSurface is supplied. The auth
// boundary is proven two ways here: /v1/readyz is asserted absent from
// UnauthenticatedRoutes(), and an anonymous request to it through the whole
// middleware stack is refused 401. The end-to-end authenticated body cases live
// in readyz_relay55_external_test.go, which drives real sessions.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dodgymike/agent-bus/internal/buscert"
)

// callReadyz invokes the handler directly, bypassing the auth middleware. The
// switch it exercises is a pure read of s.peerRoutes and s.federationExpected;
// the middleware that gates the route is proven separately below.
func callReadyz(t *testing.T, srv *Server) (*httptest.ResponseRecorder, ReadyResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, RouteReadyz, nil)
	rec := httptest.NewRecorder()
	srv.handleReadyz(rec, req)
	var body ReadyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz body is not a ReadyResponse (%v): %s", err, rec.Body.String())
	}
	return rec, body
}

// TestReadyzReportsFederationState covers the three shapes the peer surface can
// be in, and pins the premise (did the surface actually mount) for each so the
// verdict cannot pass for the wrong reason.
func TestReadyzReportsFederationState(t *testing.T) {
	t.Run("ready when the peer surface mounted", func(t *testing.T) {
		// A complete PeerSurface plus a resolver that binds the remote bus: New
		// mounts the four peer routes, so s.peerRoutes is non-empty. This is the
		// real mount, not a hand-set map.
		surface := pmSurface(t, &pmReached{})
		resolver := &pmResolver{bound: map[buscert.Fingerprint]string{
			buscert.FingerprintOf(pmCert(t, pmRemoteBus)): pmRemoteBus,
		}}
		srv, _ := pmServer(t, surface, resolver, nil)

		// Premise: the peer routes really mounted. If they did not, "ready" below
		// would be meaningless.
		if !hasPeerRoutes(srv) {
			t.Fatalf("premise broken: no peer route mounted, so this cannot test the ready case; routes=%v", srv.Routes())
		}

		rec, body := callReadyz(t, srv)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if body.Status != "ready" || body.Federation != federationReady {
			t.Fatalf("body = %+v, want {ready, %s}", body, federationReady)
		}
	})

	t.Run("unserved (503) when peer records exist but the surface did not mount", func(t *testing.T) {
		// The silently-deaf state: FederationExpected is true (records on disk) but
		// no Peer surface was supplied, so nothing mounted. This is the ONE
		// not-ready verdict, and it is what -require-federation refuses to start in.
		srv := New(Options{
			Identity:           StaticIdentity(pmLocalBus),
			FederationExpected: true,
		})
		if hasPeerRoutes(srv) {
			t.Fatalf("premise broken: a peer route mounted with no PeerSurface; routes=%v", srv.Routes())
		}

		rec, body := callReadyz(t, srv)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if body.Status != "not_ready" || body.Federation != federationUnserved {
			t.Fatalf("body = %+v, want {not_ready, %s}", body, federationUnserved)
		}
	})

	t.Run("not_configured (200) when there are no peer records", func(t *testing.T) {
		// An ordinary non-federating bus: no records, no surface. It is ready — it
		// is doing exactly what it was configured to do.
		srv := New(Options{
			Identity:           StaticIdentity(pmLocalBus),
			FederationExpected: false,
		})
		if hasPeerRoutes(srv) {
			t.Fatalf("premise broken: a peer route mounted on a non-federating build; routes=%v", srv.Routes())
		}

		rec, body := callReadyz(t, srv)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if body.Status != "ready" || body.Federation != federationNotConfigured {
			t.Fatalf("body = %+v, want {ready, %s}", body, federationNotConfigured)
		}
	})
}

// TestReadyzIsAuthenticatedAndOffTheAllowList proves the route is protected by
// being registered off the unauthenticated allow-list (RELAY-55's design choice:
// an authenticated route adds nothing to invariant 3's enumeration). It is the
// security-load-bearing test for this route.
func TestReadyzIsAuthenticatedAndOffTheAllowList(t *testing.T) {
	// It must NOT be on the allow-list. If it ever is, this fails — an operator
	// diagnostic that leaked federation state to any anonymous caller.
	for _, p := range UnauthenticatedRoutes() {
		if p == RouteReadyz {
			t.Fatalf("%s is on UnauthenticatedRoutes(); RELAY-55 requires it AUTHENTICATED, off the allow-list", RouteReadyz)
		}
	}

	// It must be a registered route: an authenticated route that was never
	// registered is a 401 for everyone, which would look like it works.
	srv := New(Options{Identity: StaticIdentity(pmLocalBus)})
	registered := false
	for _, r := range srv.Routes() {
		if r == RouteReadyz {
			registered = true
			break
		}
	}
	if !registered {
		t.Fatalf("%s is not a registered route; routes=%v", RouteReadyz, srv.Routes())
	}

	// Anonymous, through the whole middleware stack: default-deny refuses 401
	// before the handler runs, so an unauthenticated caller learns nothing about
	// federation.
	req := httptest.NewRequest(http.MethodGet, RouteReadyz, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET %s = %d, want 401 (it must be authenticated)", RouteReadyz, rec.Code)
	}
}

// TestReadyzRejectsNonGET pins the method contract: like /healthz and /v1/info,
// readyz is a pure read and answers 405 to anything but GET/HEAD.
func TestReadyzRejectsNonGET(t *testing.T) {
	srv := New(Options{Identity: StaticIdentity(pmLocalBus)})
	req := httptest.NewRequest(http.MethodPost, RouteReadyz, nil)
	rec := httptest.NewRecorder()
	srv.handleReadyz(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST %s = %d, want 405", RouteReadyz, rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != AllowGET {
		t.Fatalf("Allow = %q, want %q", got, AllowGET)
	}
}

// hasPeerRoutes reports whether any /v1/peer/ route mounted — the same predicate
// handleReadyz reads through s.peerSurfaceMounted().
func hasPeerRoutes(srv *Server) bool {
	return srv.peerSurfaceMounted()
}
