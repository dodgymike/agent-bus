package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/httpapi"
)

// testIdentity is a minimal Identity used to prove /v1/info reports the bus
// id supplied through the Identity interface rather than a hardcoded
// constant (invariant 1: the server is authoritative on ids, but here the
// point is that httpapi must not itself bake in a placeholder value).
type testIdentity string

func (t testIdentity) BusID() string { return string(t) }

// TestHealthzInfo covers CORE-3: GET /healthz and GET /v1/info.
func TestHealthzInfo(t *testing.T) {
	t.Run("routes", func(t *testing.T) {
		srv := httpapi.New(httpapi.Options{
			Identity: testIdentity("bus-under-test"),
			Version:  "v9.9.9",
		})

		// Note the two unknown-path cases: they expect 401, NOT 404. That is
		// deliberate. AUTH-2's middleware is default-deny and wraps the whole
		// mux, so an UNAUTHENTICATED caller is refused before the mux ever
		// decides whether the path exists -- it must not be able to enumerate
		// which paths this bus serves by reading status codes. A caller
		// holding a valid token passes the middleware and gets a genuine 404.
		cases := []struct {
			name       string
			method     string
			path       string
			wantStatus int
			wantAllow  string
		}{
			{"healthz get", http.MethodGet, "/healthz", http.StatusOK, ""},
			{"info get", http.MethodGet, "/v1/info", http.StatusOK, ""},
			{"healthz post", http.MethodPost, "/healthz", http.StatusMethodNotAllowed, http.MethodGet},
			{"healthz put", http.MethodPut, "/healthz", http.StatusMethodNotAllowed, http.MethodGet},
			{"healthz delete", http.MethodDelete, "/healthz", http.StatusMethodNotAllowed, http.MethodGet},
			{"info post", http.MethodPost, "/v1/info", http.StatusMethodNotAllowed, http.MethodGet},
			{"info put", http.MethodPut, "/v1/info", http.StatusMethodNotAllowed, http.MethodGet},
			{"info delete", http.MethodDelete, "/v1/info", http.StatusMethodNotAllowed, http.MethodGet},
			{"unknown path", http.MethodGet, "/nope", http.StatusUnauthorized, ""},
			{"unknown path under info", http.MethodGet, "/v1/info/nope", http.StatusUnauthorized, ""},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				// Deliberately no Authorization / credential header on any
				// case: both routes must be reachable unauthenticated.
				req := httptest.NewRequest(tc.method, tc.path, nil)
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)

				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
				}
				if tc.wantAllow != "" {
					if got := rec.Header().Get("Allow"); got != tc.wantAllow {
						t.Fatalf("Allow header = %q, want %q", got, tc.wantAllow)
					}
				}
			})
		}
	})

	t.Run("healthz body is exactly {status:ok}, no auth required", func(t *testing.T) {
		srv := httpapi.New(httpapi.Options{})
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type = %q, want prefix application/json", ct)
		}

		var got httpapi.HealthResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding body: %v (body=%q)", err, rec.Body.String())
		}
		if want := (httpapi.HealthResponse{Status: "ok"}); got != want {
			t.Fatalf("body = %+v, want %+v", got, want)
		}

		// Strict: decode into a generic map too, so an accidental extra
		// field on /healthz fails this test rather than passing silently
		// because the typed struct only looked at the fields it knew about.
		var generic map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &generic); err != nil {
			t.Fatalf("decoding generic body: %v", err)
		}
		if len(generic) != 1 || generic["status"] != "ok" {
			t.Fatalf("body fields = %v, want exactly {status: ok}", generic)
		}
	})

	t.Run("info reports injected identity, version, increasing uptime, minimal fields, no auth required", func(t *testing.T) {
		const wantBusID = "bus-injected-xyz"
		const wantVersion = "v1.2.3-test"

		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		clock := start
		now := func() time.Time { return clock }

		srv := httpapi.New(httpapi.Options{
			Identity:  testIdentity(wantBusID),
			Version:   wantVersion,
			StartedAt: start,
			Now:       now,
		})

		doInfo := func() (httpapi.InfoResponse, map[string]interface{}, *httptest.ResponseRecorder) {
			// No credential presented: /v1/info must still answer 200.
			req := httptest.NewRequest(http.MethodGet, "/v1/info", nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			var typed httpapi.InfoResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &typed); err != nil {
				t.Fatalf("decoding typed body: %v (body=%q)", err, rec.Body.String())
			}
			var generic map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &generic); err != nil {
				t.Fatalf("decoding generic body: %v (body=%q)", err, rec.Body.String())
			}
			return typed, generic, rec
		}

		clock = start.Add(1 * time.Second)
		first, firstGeneric, rec1 := doInfo()

		if rec1.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec1.Code)
		}
		if ct := rec1.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type = %q, want prefix application/json", ct)
		}
		if first.BusID != wantBusID {
			t.Fatalf("bus_id = %q, want %q (proves it comes from the injected Identity, not %q)",
				first.BusID, wantBusID, httpapi.DefaultBusID)
		}
		if first.Version != wantVersion {
			t.Fatalf("version = %q, want %q", first.Version, wantVersion)
		}
		if first.UptimeSeconds < 0 {
			t.Fatalf("uptime_seconds = %v, want >= 0", first.UptimeSeconds)
		}
		if first.UptimeSeconds < 0.5 || first.UptimeSeconds > 1.5 {
			t.Fatalf("uptime_seconds = %v, want ~1.0s (Options.Now hook not honoured?)", first.UptimeSeconds)
		}

		// Security-relevant assertion: the unauthenticated endpoint's field
		// set must stay exactly {bus_id, version, uptime_seconds}. A future
		// change that adds data-dir, listen addr, peer list or agent roster
		// must fail this test.
		wantKeys := map[string]bool{"bus_id": true, "version": true, "uptime_seconds": true}
		if len(firstGeneric) != len(wantKeys) {
			t.Fatalf("field count = %d, want %d; got fields %v", len(firstGeneric), len(wantKeys), keysOf(firstGeneric))
		}
		for k := range firstGeneric {
			if !wantKeys[k] {
				t.Fatalf("unexpected field %q leaked from unauthenticated /v1/info response: %v", k, firstGeneric)
			}
		}

		clock = start.Add(5 * time.Second)
		second, _, _ := doInfo()
		if second.UptimeSeconds <= first.UptimeSeconds {
			t.Fatalf("uptime did not increase across calls: first=%v second=%v", first.UptimeSeconds, second.UptimeSeconds)
		}
	})
}

func keysOf(m map[string]interface{}) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
