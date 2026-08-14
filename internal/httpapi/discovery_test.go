package httpapi_test

// Tests for DISCOVERY-DOC: GET /v1/discovery, the bounded, STATIC,
// UNAUTHENTICATED protocol-discovery document, plus the single constant field
// it adds to GET /v1/info.
//
// Every assertion in this file exists because the endpoint is ANONYMOUS. The
// rules discovery.go states about itself -- it describes the PROTOCOL and never
// the ROSTER, its endpoint list is a compile-time constant and not a projection
// of the mux, and its response cannot grow with bus state -- are only worth
// something if a test fails the day one of them stops being true. So the field
// sets here are EXHAUSTIVE (an added field fails, it does not slip through), the
// "static" claim is proved by BYTE-IDENTITY across differently-configured
// servers rather than by inspection, and the constants discovery.go mirrors from
// other packages are compared against the real ones.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// discoveryMaxBytes is the ceiling on the whole response. It is generous on
// purpose: the number is not a budget, the POINT is that an UNAUTHENTICATED
// response must have a FINITE, KNOWN bound. A document that can grow without
// limit is a denial-of-service surface any stranger can pull on, and growth is
// also the symptom of the failure mode this file is really guarding against --
// a document that started summarising bus state.
const discoveryMaxBytes = 16 * 1024

// getDiscovery fetches GET /v1/discovery with NO credential of any kind and
// returns the recorder. Every caller in this file relies on the absence of an
// Authorization header: the endpoint must be reachable by a stranger.
func getDiscovery(t *testing.T, srv *httpapi.Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, httpapi.RouteDiscovery, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// discoveryBody fetches the document, asserts the response envelope and returns
// the raw bytes plus the generically-decoded body. Generic, not typed: a typed
// struct silently tolerates an added field, and an added field on an anonymous
// endpoint is precisely what these tests are for.
func discoveryBody(t *testing.T, srv *httpapi.Server) ([]byte, map[string]interface{}) {
	t.Helper()
	rec := getDiscovery(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s with no credential = %d, want 200 (body=%q)", httpapi.RouteDiscovery, rec.Code, rec.Body.String())
	}
	raw := rec.Body.Bytes()
	var generic map[string]interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("decoding %s body %q: %v", httpapi.RouteDiscovery, rec.Body.String(), err)
	}
	return raw, generic
}

// newDiscoveryServer builds the plainest possible server: no auth service, no
// hub, no durable log. The document must be complete even here, because a
// caller cannot tell the difference and the endpoint list is static.
func newDiscoveryServer(t *testing.T, busID string) *httpapi.Server {
	t.Helper()
	return httpapi.New(httpapi.Options{Identity: testIdentity(busID)})
}

// exhaustiveKeys asserts that got has EXACTLY the named keys. The failure
// message is addressed to whoever reads it in a diff, because the decision it
// asks for is a security decision and not a bookkeeping one.
func exhaustiveKeys(t *testing.T, where string, got map[string]interface{}, want ...string) {
	t.Helper()
	have := keysOf(got)
	sort.Strings(have)
	sort.Strings(want)
	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Errorf("%s field set = %v, want EXACTLY %v.\n"+
			"This endpoint is UNAUTHENTICATED. Every field here is something a stranger holding nothing but a URL learns about this bus.\n"+
			"If you ADDED a field: do not update this pin until you can show the value is a compile-time constant or otherwise carries NO bus state --\n"+
			"  no agent names or ids, no counts, no on-disk paths, no listen address, no peer list, no config value, no clock, and nothing derived from which\n"+
			"  routes this build registered (see discovery.go: the endpoint list is deliberately NOT a projection of s.routes).\n"+
			"If you REMOVED a field: a client following the document may have been relying on it.",
			where, have, want)
	}
}

// asMap coerces a decoded JSON value to an object, failing loudly rather than
// letting a nil map turn an exhaustive key check into a vacuous one.
func asMap(t *testing.T, where string, v interface{}) map[string]interface{} {
	t.Helper()
	m, ok := v.(map[string]interface{})
	if !ok {
		t.Fatalf("%s = %#v, want a JSON object; the key-set assertion below would otherwise check nothing", where, v)
	}
	return m
}

// asSlice coerces a decoded JSON value to a non-empty array, for the same
// reason asMap exists: a range over an empty slice asserts nothing at all.
func asSlice(t *testing.T, where string, v interface{}) []interface{} {
	t.Helper()
	s, ok := v.([]interface{})
	if !ok {
		t.Fatalf("%s = %#v, want a JSON array", where, v)
	}
	if len(s) == 0 {
		t.Fatalf("%s is empty, so every assertion over its elements would pass vacuously", where)
	}
	return s
}

// TestDiscoveryEndpoint covers the envelope: reachable anonymously, JSON, GET
// only, and bounded.
func TestDiscoveryEndpoint(t *testing.T) {
	srv := newDiscoveryServer(t, "bus-discovery-test")

	t.Run("reachable with no credential", func(t *testing.T) {
		// Deliberately no Authorization header. A caller that has to
		// authenticate to learn HOW to authenticate is locked out forever.
		rec := getDiscovery(t, srv)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s with NO credential = %d, want 200 (body=%q); this route is on the allow-list precisely so a stranger can read it",
				httpapi.RouteDiscovery, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type = %q, want prefix application/json", ct)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
		}
	})

	t.Run("methods", func(t *testing.T) {
		cases := []struct {
			name       string
			method     string
			wantStatus int
			wantAllow  string
		}{
			// "GET, HEAD" since CORE-7: requireGET accepts HEAD, so Allow must
			// name it. HEAD's own behaviour on this route is pinned by
			// TestHeadRequest, more strongly than a row here could, so it is
			// deliberately not duplicated into this table.
			{"get", http.MethodGet, http.StatusOK, ""},
			{"post", http.MethodPost, http.StatusMethodNotAllowed, "GET, HEAD"},
			{"put", http.MethodPut, http.StatusMethodNotAllowed, "GET, HEAD"},
			{"delete", http.MethodDelete, http.StatusMethodNotAllowed, "GET, HEAD"},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, httpapi.RouteDiscovery, nil)
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)

				if rec.Code != tc.wantStatus {
					t.Fatalf("%s %s = %d, want %d (body=%q)", tc.method, httpapi.RouteDiscovery, rec.Code, tc.wantStatus, rec.Body.String())
				}
				if tc.wantAllow != "" {
					if got := rec.Header().Get("Allow"); got != tc.wantAllow {
						t.Fatalf("%s %s Allow = %q, want %q", tc.method, httpapi.RouteDiscovery, got, tc.wantAllow)
					}
				}
			})
		}
	})

	t.Run("the response is bounded", func(t *testing.T) {
		raw, _ := discoveryBody(t, srv)
		if len(raw) > discoveryMaxBytes {
			t.Fatalf("response is %d bytes, over the %d-byte ceiling.\n"+
				"An UNAUTHENTICATED response must have a finite, known bound -- an unbounded one is a DoS surface any stranger can pull on.\n"+
				"Growth is also the symptom of the real risk: check the document has not begun summarising bus state before raising the ceiling.",
				len(raw), discoveryMaxBytes)
		}
		if len(raw) == 0 {
			t.Fatal("response is empty; the assertions in this file would be checking nothing")
		}
	})
}

// TestDiscoveryFieldSetIsPinned is THE security assertion of this file: the
// exact shape of what an anonymous caller receives, top level and nested.
func TestDiscoveryFieldSetIsPinned(t *testing.T) {
	srv := newDiscoveryServer(t, "bus-discovery-test")
	_, body := discoveryBody(t, srv)

	t.Run("top level", func(t *testing.T) {
		exhaustiveKeys(t, "GET "+httpapi.RouteDiscovery, body,
			"service",
			"description",
			"bus_id",
			"paths_are_relative_to",
			"steps",
			"endpoints",
			"enrolment",
			"session",
			"client",
			"limitations",
		)
	})

	t.Run("enrolment", func(t *testing.T) {
		// invite_accepted was ADDED by INVITE-GATE, and it meets the bar the
		// failure message above sets: it is derived ONCE IN New from whether an
		// invite store was supplied, which is fixed for the process lifetime, so
		// it carries no bus state, no count, no clock and nothing derived from
		// s.routes -- /v1/enroll is registered either way. A caller learns the
		// same bit anyway the first time it presents an invite and gets 201
		// rather than 501.
		exhaustiveKeys(t, "enrolment", asMap(t, "enrolment", body["enrolment"]),
			"invite_required", "invite_accepted", "invite_note", "you_supply", "you_receive")
	})

	t.Run("session", func(t *testing.T) {
		exhaustiveKeys(t, "session", asMap(t, "session", body["session"]),
			"model", "lifetime_seconds", "refresh_after_seconds", "authorization_header", "signing_context")
	})

	t.Run("client", func(t *testing.T) {
		exhaustiveKeys(t, "client", asMap(t, "client", body["client"]),
			"binary", "build", "go_package", "note")
	})

	t.Run("every endpoint entry", func(t *testing.T) {
		endpoints := asSlice(t, "endpoints", body["endpoints"])
		for i, raw := range endpoints {
			ep := asMap(t, "endpoints["+strconv.Itoa(i)+"]", raw)
			exhaustiveKeys(t, "endpoints["+strconv.Itoa(i)+"]", ep, "name", "method", "path", "auth", "purpose")

			// An entry's `auth` is a two-value vocabulary. A third spelling
			// would be a claim a client cannot act on.
			switch ep["auth"] {
			case "none", "bearer":
			default:
				t.Errorf("endpoints[%d] (%v) auth = %#v, want \"none\" or \"bearer\"", i, ep["name"], ep["auth"])
			}
			if p, _ := ep["path"].(string); !strings.HasPrefix(p, "/") {
				t.Errorf("endpoints[%d] path = %#v, want a rooted path a client can resolve against the base URL", i, ep["path"])
			}
		}
	})

	t.Run("steps and limitations are non-empty string lists", func(t *testing.T) {
		for _, name := range []string{"steps", "limitations"} {
			for i, v := range asSlice(t, name, body[name]) {
				if s, ok := v.(string); !ok || strings.TrimSpace(s) == "" {
					t.Errorf("%s[%d] = %#v, want a non-empty string", name, i, v)
				}
			}
		}
	})
}

// TestDiscoveryBusIDComesFromIdentity proves the one bus-specific value in the
// document is the INJECTED identity and not a constant baked into httpapi.
func TestDiscoveryBusIDComesFromIdentity(t *testing.T) {
	for _, busID := range []string{"bus-alpha-9", "bus-beta-2"} {
		busID := busID
		t.Run(busID, func(t *testing.T) {
			_, body := discoveryBody(t, newDiscoveryServer(t, busID))
			if got := body["bus_id"]; got != busID {
				t.Fatalf("bus_id = %#v, want %q (proves it comes from the injected Identity, not the %q placeholder)",
					got, busID, httpapi.DefaultBusID)
			}
		})
	}
}

// TestDiscoveryDocumentIsStatic is the DoS/leak assertion: the response does not
// grow with, or vary with, ANYTHING about this bus except its id.
//
// Byte-identity is the assertion on purpose. A field-by-field comparison only
// catches the differences the author thought to look for; identical BYTES catch
// the field nobody anticipated -- and, critically, prove the document does not
// leak this build's CONFIGURATION. discovery.go's endpoint list is static rather
// than a projection of s.routes for exactly that reason: the messaging and
// credential routes are registered only when Options.Hub and Options.Auth are
// non-nil, so a mux-derived list would hand an anonymous caller the wiring that
// authMiddleware's 401-not-404 choice exists to withhold.
func TestDiscoveryDocumentIsStatic(t *testing.T) {
	// The bus id is the ONE thing every server here shares, because it is the
	// one thing the document is allowed to depend on.
	const busID = msgTestBusID

	walDir := t.TempDir()
	walLog, err := wal.Open(wal.LogOptions{Dir: walDir})
	if err != nil {
		t.Fatalf("opening a write-ahead log in %q: %v", walDir, err)
	}
	t.Cleanup(func() {
		if err := walLog.Close(); err != nil {
			t.Errorf("closing the write-ahead log: %v", err)
		}
	})

	// A fully-wired bus: real auth service, real hub, real durable log, every
	// messaging route registered. Its bus id is msgTestBusID, which is why the
	// bare servers above use the same one.
	fullyWired, _ := newMessagingServer(t)
	if fullyWired.Auth() == nil || fullyWired.Hub() == nil {
		t.Fatal("the fully-wired server has no auth service or no hub, so it is not actually a different configuration and this test would prove nothing")
	}

	epoch := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	later := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	servers := []struct {
		name string
		srv  *httpapi.Server
	}{
		{"bare, zero options but the identity", httpapi.New(httpapi.Options{Identity: testIdentity(busID)})},
		{"different version and clock", httpapi.New(httpapi.Options{
			Identity:  testIdentity(busID),
			Version:   "v99.99.99-totally-different",
			StartedAt: epoch,
			Now:       func() time.Time { return later },
		})},
		{"with a real durable log", httpapi.New(httpapi.Options{
			Identity: testIdentity(busID),
			Durable:  walLog,
		})},
		{"with a long poll timeout", httpapi.New(httpapi.Options{
			Identity:    testIdentity(busID),
			PollTimeout: 90 * time.Second,
		})},
		{"fully wired: auth + hub + durable, every messaging route registered", fullyWired},
	}

	// Sanity: the servers really are configured differently, or byte-identity
	// below would be trivially true. The bare server registers only the three
	// always-on routes; the fully-wired one registers the credential and
	// messaging surface too.
	if len(servers[0].srv.Routes()) >= len(fullyWired.Routes()) {
		t.Fatalf("the bare server registered %v and the fully-wired one %v; they are not meaningfully different configurations, so this test proves nothing",
			servers[0].srv.Routes(), fullyWired.Routes())
	}

	want, _ := discoveryBody(t, servers[0].srv)
	for _, s := range servers[1:] {
		got, _ := discoveryBody(t, s.srv)
		if string(got) != string(want) {
			t.Errorf("the discovery document differs between %q and %q.\n"+
				"It MUST be byte-identical for a given bus id: it is built once in New from the bus id alone and must not vary with version, clock,\n"+
				"durable log, poll timeout, or which routes this build registered. A difference here means the document has started describing THIS\n"+
				"INSTANCE rather than the protocol -- which hands an anonymous caller the configuration the 401-not-404 choice exists to withhold.\n"+
				"  %s:\n    %s\n  %s:\n    %s",
				servers[0].name, s.name, servers[0].name, want, s.name, got)
		}
	}
}

// TestDiscoveryRepeatCallsAreIdentical pins that the handler computes nothing:
// no clock, no counter, no per-request state.
func TestDiscoveryRepeatCallsAreIdentical(t *testing.T) {
	srv := newDiscoveryServer(t, "bus-discovery-test")

	first, _ := discoveryBody(t, srv)
	for i := 0; i < 5; i++ {
		got, _ := discoveryBody(t, srv)
		if string(got) != string(first) {
			t.Fatalf("call %d differs from the first; the handler must write a value built once in New and compute nothing per request.\n  first: %s\n  call %d: %s",
				i+2, first, i+2, got)
		}
	}
}

// TestDiscoveryDocumentLeaksNoBusState is the "protocol, not roster" line, made
// checkable: whatever the wording, these strings must not be in the body.
func TestDiscoveryDocumentLeaksNoBusState(t *testing.T) {
	t.Run("no enrolled agent appears in the document", func(t *testing.T) {
		srv, _ := newMessagingServer(t)

		// Distinctive names, so a hit is unambiguous rather than an accidental
		// substring of ordinary prose.
		_, _, pubA := newAuthKeypair(t)
		_, _, pubB := newAuthKeypair(t)
		idA := enrolOverHTTP(t, srv, "zqcanary", pubA, "idem-discovery-a")
		idB := enrolOverHTTP(t, srv, "zqbadger", pubB, "idem-discovery-b")

		raw, _ := discoveryBody(t, srv)
		body := string(raw)
		for _, secret := range []struct{ what, value string }{
			{"an enrolled agent's minted id", idA},
			{"an enrolled agent's minted id", idB},
			{"an enrolled agent's name", "zqcanary"},
			{"an enrolled agent's name", "zqbadger"},
			{"an enrolled agent's public key", pubA},
			{"an enrolled agent's public key", pubB},
		} {
			if strings.Contains(body, secret.value) {
				t.Errorf("the discovery document contains %s (%q).\n"+
					"This endpoint is ANONYMOUS and describes the PROTOCOL, never the ROSTER: who is enrolled here is exactly what /v1/agents requires a bearer token for.",
					secret.what, secret.value)
			}
		}
	})

	t.Run("no on-disk path appears in the document", func(t *testing.T) {
		dir := t.TempDir()
		walLog, err := wal.Open(wal.LogOptions{Dir: dir})
		if err != nil {
			t.Fatalf("opening a write-ahead log in %q: %v", dir, err)
		}
		t.Cleanup(func() {
			if err := walLog.Close(); err != nil {
				t.Errorf("closing the write-ahead log: %v", err)
			}
		})

		srv := httpapi.New(httpapi.Options{Identity: testIdentity("bus-discovery-test"), Durable: walLog})
		raw, _ := discoveryBody(t, srv)
		body := string(raw)

		for _, p := range []string{dir, walLog.Path()} {
			if strings.Contains(body, p) {
				t.Errorf("the discovery document contains the on-disk path %q; where this bus keeps its data is not a protocol fact and is no stranger's business", p)
			}
		}
	})

	t.Run("the session signing prefix is never served", func(t *testing.T) {
		// THE one that matters most. internal/auth/session.go documents
		// SessionSigningContext as a value the CLIENT MUST PIN: a client that
		// learned the domain-separation prefix from the server would sign
		// whatever a man in the middle chose to put in front of the token.
		// Asserted against the real constant, not a copy, so a change to the
		// prefix cannot leave this test guarding a stale string.
		srv, _ := newMessagingServer(t)
		raw, _ := discoveryBody(t, srv)
		if strings.Contains(string(raw), auth.SessionSigningContext) {
			t.Fatalf("the discovery document serves auth.SessionSigningContext (%q).\n"+
				"It must NEVER go on the wire: the client pins it, precisely so a man in the middle cannot choose the bytes the client signs.\n"+
				"session.signing_context is meant to SAY it is not served and point at the compiled client. body=%s",
				auth.SessionSigningContext, raw)
		}

		// And the field that stands in its place must actually be there and
		// must not be quietly emptied into something a client would read as
		// "no prefix".
		_, body := discoveryBody(t, srv)
		sess := asMap(t, "session", body["session"])
		note, _ := sess["signing_context"].(string)
		if strings.TrimSpace(note) == "" {
			t.Error("session.signing_context is empty; it must explain that the prefix is deliberately NOT served and where to take it from instead")
		}
	})
}

// TestDiscoveryPathsMatchRouteConstants pins the document to the mux. discovery.go
// builds its endpoint list from the Route* constants; this asserts the SERVED
// values still equal them, so a route rename cannot silently desync the document
// from the surface it describes.
func TestDiscoveryPathsMatchRouteConstants(t *testing.T) {
	_, body := discoveryBody(t, newDiscoveryServer(t, "bus-discovery-test"))

	byName := map[string]map[string]interface{}{}
	for i, raw := range asSlice(t, "endpoints", body["endpoints"]) {
		ep := asMap(t, "endpoints["+strconv.Itoa(i)+"]", raw)
		name, _ := ep["name"].(string)
		if name == "" {
			t.Fatalf("endpoints[%d] has no name: %v", i, ep)
		}
		if _, dup := byName[name]; dup {
			t.Fatalf("endpoints has two entries named %q; a client keying off the name would see only one", name)
		}
		byName[name] = ep
	}

	cases := []struct {
		name     string
		wantPath string
		wantAuth string
	}{
		// /healthz and /v1/info are literals in discovery.go only because
		// neither has a Route* constant today; they are spelled out here so a
		// typo in either still fails.
		{"discovery", httpapi.RouteDiscovery, "none"},
		{"info", "/v1/info", "none"},
		{"healthz", "/healthz", "none"},
		{"enroll", httpapi.RouteEnroll, "none"},
		{"session-begin", httpapi.RouteSessionBegin, "none"},
		{"session-complete", httpapi.RouteSessionComplete, "none"},
		{"agents", httpapi.RouteAgents, "bearer"},
		{"mint", httpapi.RouteMint, "bearer"},
		{"send", httpapi.RouteSend, "bearer"},
		{"messages", httpapi.RouteMessages, "bearer"},
		{"wait", httpapi.RouteWait, "bearer"},
	}

	if len(byName) != len(cases) {
		t.Errorf("the document lists %d endpoints, this test knows %d.\n"+
			"An endpoint added to or removed from discovery.go must be added to or removed from this table, so the document and the mux cannot drift apart unnoticed.\n"+
			"served: %v",
			len(byName), len(cases), keysOfEndpoints(byName))
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ep, ok := byName[tc.name]
			if !ok {
				t.Fatalf("no endpoint named %q in the document (have %v)", tc.name, keysOfEndpoints(byName))
			}
			if got := ep["path"]; got != tc.wantPath {
				t.Fatalf("endpoint %q path = %#v, want %q; the document and the mux have desynced -- was a Route* constant renamed?", tc.name, got, tc.wantPath)
			}
			if got := ep["auth"]; got != tc.wantAuth {
				t.Fatalf("endpoint %q auth = %#v, want %q; the document must not misstate whether a route needs a credential", tc.name, got, tc.wantAuth)
			}
		})
	}

	t.Run("broadcast is not advertised", func(t *testing.T) {
		// POST /v1/broadcast answers 501 on this build. Advertising a route
		// that refuses everything is worse than not advertising it; the
		// limitations list covers it honestly instead.
		for name, ep := range byName {
			if ep["path"] == httpapi.RouteBroadcast {
				t.Fatalf("endpoint %q advertises %s, which answers 501 on this build", name, httpapi.RouteBroadcast)
			}
		}
		if !strings.Contains(strings.ToLower(rawOf(t, body["limitations"])), strings.ToLower(httpapi.RouteBroadcast)) {
			t.Errorf("%s is neither advertised nor named in limitations; a reader is left to discover the 501 by hitting it", httpapi.RouteBroadcast)
		}

		// And pin the CLAIM, not just the wording. Asserting only that the
		// limitations text mentions the route lets the document keep saying
		// "broadcast is refused" long after SIGN-3 un-refuses it. This is the
		// assertion that fails on the day it stops being true, which is the day
		// the document must be rewritten and the route advertised instead.
		srv, _ := newMessagingServer(t)
		sender := enrolAndAuthenticate(t, srv, "discovery-broadcast-probe")
		rec := authed(t, srv, sender, http.MethodPost, httpapi.RouteBroadcast, `{}`)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("POST %s = %d, want 501 -- discovery limitation 4 claims this route is REFUSED, and that claim is now false (body=%q)",
				httpapi.RouteBroadcast, rec.Code, rec.Body.String())
		}
	})
}

// TestDiscoverySessionConstantsMatchAuth closes the desync the implementer
// flagged: discovery.go mirrors auth.SessionLifetime and auth.RefreshAfter() as
// bare integer literals (3600 / 2700) with only a comment naming the source of
// truth. A comment does not fail a build. This does.
func TestDiscoverySessionConstantsMatchAuth(t *testing.T) {
	_, body := discoveryBody(t, newDiscoveryServer(t, "bus-discovery-test"))
	sess := asMap(t, "session", body["session"])

	cases := []struct {
		field string
		want  int
		src   string
	}{
		{"lifetime_seconds", int(auth.SessionLifetime / time.Second), "auth.SessionLifetime"},
		{"refresh_after_seconds", int(auth.RefreshAfter() / time.Second), "auth.RefreshAfter()"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.field, func(t *testing.T) {
			got, ok := sess[tc.field].(float64) // JSON numbers decode as float64
			if !ok {
				t.Fatalf("session.%s = %#v, want a number", tc.field, sess[tc.field])
			}
			if int(got) != tc.want {
				t.Fatalf("session.%s = %d, want %d (= %s).\n"+
					"discovery.go mirrors this as a LITERAL with a comment naming the source of truth. The comment cannot fail a build, so this test is the thing\n"+
					"that stops the document telling clients a session lasts longer than the server will honour -- change the literal in discovery.go to match.",
					tc.field, int(got), tc.want, tc.src)
			}
		})
	}

	t.Run("refresh comes before expiry", func(t *testing.T) {
		lifetime, _ := sess["lifetime_seconds"].(float64)
		refresh, _ := sess["refresh_after_seconds"].(float64)
		if refresh <= 0 || refresh >= lifetime {
			t.Fatalf("refresh_after_seconds = %v, lifetime_seconds = %v; a client told to refresh at or after expiry has no overlap and will be briefly credential-less", refresh, lifetime)
		}
	})
}

// TestDiscoveryEnrolmentIsHonest pins the document to what this build actually
// enforces. A document that claims a control it does not have is a FALSE
// SECURITY CLAIM -- worse than saying nothing, because a reader makes a trust
// decision on it.
func TestDiscoveryEnrolmentIsHonest(t *testing.T) {
	_, body := discoveryBody(t, newDiscoveryServer(t, "bus-discovery-test"))
	enrolment := asMap(t, "enrolment", body["enrolment"])

	t.Run("invite_required is false", func(t *testing.T) {
		// FALSE is the truthful value TODAY: invite-gated enrolment (INVITE-GATE)
		// is still `todo` in the backlog and POST /v1/enroll has no invite field.
		//
		// WHEN INVITE-GATE LANDS, THIS TEST IS THE THING THAT MUST BE UPDATED IN
		// THE SAME TASK -- flip the expectation to true there, in the commit that
		// makes it true, so the document can never be ahead of the enforcement.
		// Do not "fix" this test by loosening it.
		if got := enrolment["invite_required"]; got != false {
			t.Fatalf("enrolment.invite_required = %#v, want false.\n"+
				"If enrolment is genuinely invite-gated now, this is the right change -- but make it in the SAME task as the gate, never before it.\n"+
				"If it is not, the document is making a security claim this build cannot keep.",
				got)
		}
	})

	t.Run("invite_note does not claim invites are live", func(t *testing.T) {
		note, _ := enrolment["invite_note"].(string)
		if strings.TrimSpace(note) == "" {
			t.Fatal("enrolment.invite_note is empty; a reader is left to infer whether enrolment is gated")
		}
		lower := strings.ToLower(note)
		// The note must say enrolment is OPEN. Checking for the honest word is
		// worth more than trying to enumerate every dishonest phrasing.
		if !strings.Contains(lower, "open") {
			t.Errorf("enrolment.invite_note = %q; while invite_required is false it must state plainly that enrolment is OPEN to any caller that can reach this bus", note)
		}
		for _, claim := range []string{"invite required", "invite is required", "requires an invite", "invite-only enrolment is enforced"} {
			if strings.Contains(lower, claim) {
				t.Errorf("enrolment.invite_note = %q contains %q, which reads as a control this build does not have (invite_required is false)", note, claim)
			}
		}
	})
}

// TestDiscoveryPointerOnInfo pins the single field DISCOVERY-DOC added to
// /v1/info: a constant path that actually resolves.
func TestDiscoveryPointerOnInfo(t *testing.T) {
	srv := newDiscoveryServer(t, "bus-discovery-test")

	req := httptest.NewRequest(http.MethodGet, "/v1/info", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/info = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	info := decodeBody(t, rec)

	got, _ := info["discovery"].(string)
	if got != httpapi.RouteDiscovery {
		t.Fatalf("/v1/info discovery = %#v, want %q (the compile-time constant httpapi.RouteDiscovery)", info["discovery"], httpapi.RouteDiscovery)
	}
	if !strings.HasPrefix(got, "/") {
		t.Fatalf("/v1/info discovery = %q, want a rooted path a client can resolve against the base URL it already trusts", got)
	}

	// The pointer must actually resolve, or it is worse than absent: it sends a
	// caller that only knows /v1/info to a 404.
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, got, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("following /v1/info's discovery pointer (GET %s, no credential) = %d, want 200; the pointer does not resolve", got, rec2.Code)
	}
}

// keysOfEndpoints lists the endpoint names in a document, for failure messages.
func keysOfEndpoints(m map[string]map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// rawOf re-encodes a decoded JSON value so a test can substring-search it.
func rawOf(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-encoding %#v: %v", v, err)
	}
	return string(b)
}
