package httpapi_test

// RELAY-55: GET /v1/readyz end-to-end, driven through a REAL session.
//
// The white-box tests in readyz_relay55_test.go prove the switch and the auth
// boundary. These prove the operator's actual experience: an enrolled agent
// holding a live bearer token GETs /v1/readyz and reads the federation verdict
// through the whole middleware stack, and an anonymous caller is refused.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// newReadyzServer builds a server that serves the credential routes plus
// /v1/readyz, with FederationExpected set as requested and NO peer surface
// mounted. That is the reachable "records present, ingress absent" shape when
// federationExpected is true, and the ordinary non-federating shape when false.
func newReadyzServer(t *testing.T, federationExpected bool) *httpapi.Server {
	t.Helper()

	dir := t.TempDir()
	logger := logging.New(&bytes.Buffer{}, logging.LevelDebug)

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
	svc, err := auth.NewService(auth.Options{Minter: minter, Roster: auth.NewMemoryRoster()})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}

	return httpapi.New(httpapi.Options{
		Identity:           testIdentity(msgTestBusID),
		Logger:             logger,
		Durable:            walLog,
		Auth:               svc,
		FederationExpected: federationExpected,
	})
}

func TestReadyzAuthenticatedEndToEnd(t *testing.T) {
	t.Run("authenticated caller reads unserved (503) when federation was expected", func(t *testing.T) {
		srv := newReadyzServer(t, true)
		agent := enrolAndAuthenticate(t, srv, "operator")

		rec := authed(t, srv, agent, http.MethodGet, httpapi.RouteReadyz, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET /v1/readyz = %d, want 503; body %s", rec.Code, rec.Body.String())
		}
		var body httpapi.ReadyResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not a ReadyResponse (%v): %s", err, rec.Body.String())
		}
		if body.Status != "not_ready" || body.Federation != "unserved" {
			t.Fatalf("body = %+v, want {not_ready, unserved}", body)
		}
	})

	t.Run("authenticated caller reads not_configured (200) on a non-federating bus", func(t *testing.T) {
		srv := newReadyzServer(t, false)
		agent := enrolAndAuthenticate(t, srv, "operator")

		rec := authed(t, srv, agent, http.MethodGet, httpapi.RouteReadyz, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/readyz = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
		var body httpapi.ReadyResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not a ReadyResponse (%v): %s", err, rec.Body.String())
		}
		if body.Status != "ready" || body.Federation != "not_configured" {
			t.Fatalf("body = %+v, want {ready, not_configured}", body)
		}
	})

	t.Run("anonymous caller is refused 401", func(t *testing.T) {
		srv := newReadyzServer(t, true)
		req := httptest.NewRequest(http.MethodGet, httpapi.RouteReadyz, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous GET /v1/readyz = %d, want 401 (it is off the allow-list)", rec.Code)
		}
	})
}
