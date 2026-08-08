package httpapi_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// authTestBusID qualifies every agent id these tests expect (invariant 2).
const authTestBusID = "bus-http-test"

// newAuthServer builds a Server wired to a REAL *auth.Service. The service is
// real rather than a double on purpose: these tests are about the CONTRACT the
// three credential-issuing routes present — status codes, exact JSON field
// names, headers — and a stub would let the handler and the service disagree
// without any test noticing.
//
// The returned buffer captures every log record the server emits, at debug
// level, so a test can assert on what did NOT reach it.
func newAuthServer(t *testing.T) (*httpapi.Server, *bytes.Buffer) {
	t.Helper()

	minter, err := ids.NewAgentIDMinter(authTestBusID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	svc, err := auth.NewService(auth.Options{Minter: minter})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}

	var logBuf bytes.Buffer
	srv := httpapi.New(httpapi.Options{
		Identity: testIdentity(authTestBusID),
		Logger:   logging.New(&logBuf, logging.LevelDebug),
		Auth:     svc,
	})
	return srv, &logBuf
}

// postJSON issues a POST with an application/json body and returns the recorded
// response.
func postJSON(t *testing.T, srv *httpapi.Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, path, body, "application/json")
}

// doRequest issues an arbitrary request, sending contentType only when it is
// non-empty so a test can exercise the missing-header case.
func doRequest(t *testing.T, srv *httpapi.Server, method, path, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// decodeBody parses a JSON response body into a generic map so a test can
// assert on the EXACT set of field names a client will see. A typed struct
// would silently tolerate a renamed or added field, which is the thing most
// likely to break a wrapper.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decoding response body %q: %v", rec.Body.String(), err)
	}
	return m
}

func wantKeys(t *testing.T, got map[string]interface{}, want ...string) {
	t.Helper()
	have := keysOf(got)
	sort.Strings(have)
	sort.Strings(want)
	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Errorf("response fields = %v, want exactly %v", have, want)
	}
}

// newAuthKeypair returns a real keypair and the base64 spelling of the public
// half, exactly as a client sends it.
func newAuthKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating an ed25519 keypair: %v", err)
	}
	return pub, priv, base64.StdEncoding.EncodeToString(pub)
}

// enrolOverHTTP enrols name through the route and returns the minted id.
func enrolOverHTTP(t *testing.T, srv *httpapi.Server, name, pubB64, idemKey string) string {
	t.Helper()
	rec := postJSON(t, srv, httpapi.RouteEnroll, `{"name":"`+name+`","public_key":"`+pubB64+`","idempotency_key":"`+idemKey+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enrol status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	id, _ := body["agent_id"].(string)
	if id == "" {
		t.Fatalf("enrol response has no agent_id: %s", rec.Body.String())
	}
	return id
}

// TestEnrollRoute covers the contract of POST /v1/enroll: the created status,
// the exact field names, and both halves of the idempotency rule (invariant
// 10) as they appear on the wire.
func TestEnrollRoute(t *testing.T) {
	t.Run("201 with the documented field names", func(t *testing.T) {
		srv, _ := newAuthServer(t)
		_, _, pubB64 := newAuthKeypair(t)

		rec := postJSON(t, srv, httpapi.RouteEnroll, `{"name":"alpha","public_key":"`+pubB64+`","idempotency_key":"idem-1"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
		}
		if ct := rec.Result().Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		body := decodeBody(t, rec)
		wantKeys(t, body, "agent_id", "bus_id", "name", "enrolled_at")

		if want := authTestBusID + ".alpha-1"; body["agent_id"] != want {
			t.Errorf("agent_id = %v, want %q", body["agent_id"], want)
		}
		if body["bus_id"] != authTestBusID {
			t.Errorf("bus_id = %v, want %q", body["bus_id"], authTestBusID)
		}
		if body["name"] != "alpha" {
			t.Errorf("name = %v, want %q", body["name"], "alpha")
		}
		enrolledAt, _ := body["enrolled_at"].(string)
		if _, err := time.Parse(time.RFC3339Nano, enrolledAt); err != nil {
			t.Errorf("enrolled_at = %q is not RFC3339: %v", enrolledAt, err)
		}
		if rec.Result().Header.Get(httpapi.IdempotencyReplayedHeader) != "" {
			t.Error("a fresh enrolment must not be marked as replayed")
		}
	})

	t.Run("a retry replays a byte-identical body and is flagged out of band", func(t *testing.T) {
		srv, _ := newAuthServer(t)
		_, _, pubB64 := newAuthKeypair(t)
		req := `{"name":"alpha","public_key":"` + pubB64 + `","idempotency_key":"idem-1"}`

		first := postJSON(t, srv, httpapi.RouteEnroll, req)
		if first.Code != http.StatusCreated {
			t.Fatalf("first status = %d, want 201; body %s", first.Code, first.Body.String())
		}

		second := postJSON(t, srv, httpapi.RouteEnroll, req)
		if second.Code != http.StatusCreated {
			t.Fatalf("retry status = %d, want 201: the response to a retry is the response to the original, status included; body %s", second.Code, second.Body.String())
		}
		if got, want := second.Body.String(), first.Body.String(); got != want {
			t.Fatalf("retry body = %q, want the original byte for byte %q; a retry must not be able to tell the difference in the payload it parses", got, want)
		}
		if got := second.Result().Header.Get(httpapi.IdempotencyReplayedHeader); got != "true" {
			t.Errorf("%s = %q, want %q", httpapi.IdempotencyReplayedHeader, got, "true")
		}
	})

	// NARROWED 2026-08-07: this was "409 and disconnects". It is now 409 and the
	// connection is KEPT — reusing one's own key with different content is a
	// client bug, and /v1/enroll is unauthenticated, so the socket is not even a
	// proxy for an identity. The disconnect moved to the paths where a THIRD
	// PARTY's signed material is presented; see disconnect_socket_test.go, which
	// proves both halves at the socket rather than from this header.
	t.Run("the same key with a different payload is 409 and keeps the connection", func(t *testing.T) {
		srv, _ := newAuthServer(t)
		_, _, pubB64 := newAuthKeypair(t)
		_, _, otherB64 := newAuthKeypair(t)

		if rec := postJSON(t, srv, httpapi.RouteEnroll, `{"name":"alpha","public_key":"`+pubB64+`","idempotency_key":"idem-1"}`); rec.Code != http.StatusCreated {
			t.Fatalf("first status = %d, want 201", rec.Code)
		}

		for _, tc := range []struct {
			name string
			body string
		}{
			{"different name", `{"name":"beta","public_key":"` + pubB64 + `","idempotency_key":"idem-1"}`},
			{"different public key", `{"name":"alpha","public_key":"` + otherB64 + `","idempotency_key":"idem-1"}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := postJSON(t, srv, httpapi.RouteEnroll, tc.body)
				if rec.Code != http.StatusConflict {
					t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
				}
				if got := rec.Result().Header.Get("Connection"); strings.EqualFold(got, "close") {
					t.Errorf("Connection = %q, want it absent: reusing a key for new content is rejected and logged, but the caller is confused rather than hostile and its connection is kept", got)
				}
				body := decodeBody(t, rec)
				wantKeys(t, body, "error")
			})
		}
	})

	t.Run("a rejected enrolment never echoes the key material back", func(t *testing.T) {
		srv, _ := newAuthServer(t)
		_, _, pubB64 := newAuthKeypair(t)

		rec := postJSON(t, srv, httpapi.RouteEnroll, `{"name":"ALPHA","public_key":"`+pubB64+`","idempotency_key":"idem-1"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for an uppercase name; body %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), pubB64) {
			t.Error("the error body echoed the presented public key")
		}
	})
}

// TestEnrollThenSessionRoutesRoundTrip is the full HTTP exchange with a real
// keypair, as a wrapper performs it: enrol, ask for a challenge, sign
// auth.SessionSigningContext + token, complete.
func TestEnrollThenSessionRoutesRoundTrip(t *testing.T) {
	srv, _ := newAuthServer(t)
	_, priv, pubB64 := newAuthKeypair(t)
	agentID := enrolOverHTTP(t, srv, "alpha", pubB64, "idem-1")

	beginRec := postJSON(t, srv, httpapi.RouteSessionBegin, `{"agent_id":"`+agentID+`"}`)
	if beginRec.Code != http.StatusOK {
		t.Fatalf("begin status = %d, want 200; body %s", beginRec.Code, beginRec.Body.String())
	}
	beginBody := decodeBody(t, beginRec)
	wantKeys(t, beginBody, "agent_id", "token", "challenge_expires_at")

	if beginBody["agent_id"] != agentID {
		t.Errorf("agent_id = %v, want %q", beginBody["agent_id"], agentID)
	}
	token, _ := beginBody["token"].(string)
	if token == "" {
		t.Fatal("begin returned an empty token")
	}
	if _, err := time.Parse(time.RFC3339Nano, beginBody["challenge_expires_at"].(string)); err != nil {
		t.Errorf("challenge_expires_at is not RFC3339: %v", err)
	}
	if strings.Contains(beginRec.Body.String(), auth.SessionSigningContext) {
		t.Error("the begin response carries the signing context; the client must PIN that constant, or a man in the middle chooses the prefix it signs")
	}

	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(auth.SessionSigningContext+token)))
	completeRec := postJSON(t, srv, httpapi.RouteSessionComplete, `{"token":"`+token+`","signature":"`+sig+`"}`)
	if completeRec.Code != http.StatusOK {
		t.Fatalf("complete status = %d, want 200; body %s", completeRec.Code, completeRec.Body.String())
	}
	completeBody := decodeBody(t, completeRec)
	wantKeys(t, completeBody, "agent_id", "expires_at", "lifetime_seconds", "refresh_after_seconds")

	if completeBody["agent_id"] != agentID {
		t.Errorf("agent_id = %v, want %q", completeBody["agent_id"], agentID)
	}
	if got, want := completeBody["lifetime_seconds"], float64(auth.SessionLifetime/time.Second); got != want {
		t.Errorf("lifetime_seconds = %v, want %v", got, want)
	}
	if got, want := completeBody["refresh_after_seconds"], float64(auth.RefreshAfter()/time.Second); got != want {
		t.Errorf("refresh_after_seconds = %v, want %v", got, want)
	}
	expiresAt, _ := completeBody["expires_at"].(string)
	if _, err := time.Parse(time.RFC3339Nano, expiresAt); err != nil {
		t.Errorf("expires_at = %q is not RFC3339: %v", expiresAt, err)
	}

	t.Run("the session token is a live credential the service resolves", func(t *testing.T) {
		p, err := srv.Auth().Authenticate(token)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if p.AgentID != agentID {
			t.Errorf("principal agent id = %q, want %q", p.AgentID, agentID)
		}
	})

	t.Run("re-completing returns the SAME expiry", func(t *testing.T) {
		again := postJSON(t, srv, httpapi.RouteSessionComplete, `{"token":"`+token+`","signature":"`+sig+`"}`)
		if again.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", again.Code, again.Body.String())
		}
		if got := decodeBody(t, again)["expires_at"]; got != expiresAt {
			t.Fatalf("expires_at = %v on re-completion, want the original %q: one signature must never hold a session open indefinitely", got, expiresAt)
		}
	})
}

// TestEnrollThenSessionRouteStatusMapping pins the status code a client sees
// for each failure class. The mapping is by sentinel inside the handler, so
// these assertions are what stops a refactor silently turning a 401 into a 400.
func TestEnrollThenSessionRouteStatusMapping(t *testing.T) {
	t.Run("validation and identity failures", func(t *testing.T) {
		srv, _ := newAuthServer(t)
		_, _, pubB64 := newAuthKeypair(t)
		agentID := enrolOverHTTP(t, srv, "alpha", pubB64, "idem-1")

		beginRec := postJSON(t, srv, httpapi.RouteSessionBegin, `{"agent_id":"`+agentID+`"}`)
		if beginRec.Code != http.StatusOK {
			t.Fatalf("begin status = %d, want 200", beginRec.Code)
		}
		token := decodeBody(t, beginRec)["token"].(string)

		cases := []struct {
			name       string
			path       string
			body       string
			wantStatus int
		}{
			{"public key is not base64", httpapi.RouteEnroll, `{"name":"beta","public_key":"not!base64","idempotency_key":"idem-2"}`, http.StatusBadRequest},
			{"public key is base64 of the wrong length", httpapi.RouteEnroll, `{"name":"beta","public_key":"` + base64.StdEncoding.EncodeToString(make([]byte, 31)) + `","idempotency_key":"idem-2"}`, http.StatusBadRequest},
			{"public key is empty", httpapi.RouteEnroll, `{"name":"beta","public_key":"","idempotency_key":"idem-2"}`, http.StatusBadRequest},
			{"name is invalid", httpapi.RouteEnroll, `{"name":"BETA","public_key":"` + pubB64 + `","idempotency_key":"idem-2"}`, http.StatusBadRequest},
			{"idempotency key is missing", httpapi.RouteEnroll, `{"name":"beta","public_key":"` + pubB64 + `","idempotency_key":""}`, http.StatusBadRequest},
			{"unknown agent", httpapi.RouteSessionBegin, `{"agent_id":"` + authTestBusID + `.ghost-1"}`, http.StatusNotFound},
			{"malformed agent id", httpapi.RouteSessionBegin, `{"agent_id":"nonsense"}`, http.StatusNotFound},
			{"unknown session token", httpapi.RouteSessionComplete, `{"token":"nope","signature":"` + base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)) + `"}`, http.StatusNotFound},
			{"signature is not base64", httpapi.RouteSessionComplete, `{"token":"` + token + `","signature":"not!base64"}`, http.StatusBadRequest},
			{"signature does not verify", httpapi.RouteSessionComplete, `{"token":"` + token + `","signature":"` + base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)) + `"}`, http.StatusUnauthorized},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec := postJSON(t, srv, tc.path, tc.body)
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body.String())
				}
				wantKeys(t, decodeBody(t, rec), "error")
			})
		}
	})

	t.Run("wrong method is 405 with an Allow header", func(t *testing.T) {
		srv, _ := newAuthServer(t)
		for _, path := range []string{httpapi.RouteEnroll, httpapi.RouteSessionBegin, httpapi.RouteSessionComplete} {
			for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
				rec := doRequest(t, srv, method, path, "", "application/json")
				if rec.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s status = %d, want 405", method, path, rec.Code)
				}
				if got := rec.Result().Header.Get("Allow"); got != http.MethodPost {
					t.Errorf("%s %s Allow = %q, want %q", method, path, got, http.MethodPost)
				}
			}
		}
	})

	t.Run("Content-Type must be JSON", func(t *testing.T) {
		_, _, pubB64 := newAuthKeypair(t)
		body := `{"name":"alpha","public_key":"` + pubB64 + `","idempotency_key":"idem-1"}`

		cases := []struct {
			name        string
			contentType string
			wantStatus  int
		}{
			{"missing", "", http.StatusUnsupportedMediaType},
			{"text/plain", "text/plain", http.StatusUnsupportedMediaType},
			{"form post", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
			{"json-ish", "application/json-ish", http.StatusUnsupportedMediaType},
			{"json", "application/json", http.StatusCreated},
			{"json with a charset parameter", "application/json; charset=utf-8", http.StatusCreated},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				// A fresh server per case so the two accepted spellings do not
				// collide on the idempotency key.
				srv, _ := newAuthServer(t)
				rec := doRequest(t, srv, http.MethodPost, httpapi.RouteEnroll, body, tc.contentType)
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body.String())
				}
			})
		}
	})

	t.Run("strict body parsing", func(t *testing.T) {
		srv, _ := newAuthServer(t)
		_, _, pubB64 := newAuthKeypair(t)

		cases := []struct {
			name       string
			body       string
			wantStatus int
			because    string
		}{
			{
				name:       "unknown field",
				body:       `{"name":"alpha","public_key":"` + pubB64 + `","idempotency_key":"idem-1","extra":1}`,
				wantStatus: http.StatusBadRequest,
				because:    "a client that misspells public_key must get an error, not a silent enrolment with an empty key",
			},
			{
				name:       "misspelled field",
				body:       `{"name":"alpha","publickey":"` + pubB64 + `","idempotency_key":"idem-1"}`,
				wantStatus: http.StatusBadRequest,
				because:    "same class as above, and the one that actually happens",
			},
			{
				name:       "trailing content after the JSON value",
				body:       `{"name":"alpha","public_key":"` + pubB64 + `","idempotency_key":"idem-1"}{"name":"beta"}`,
				wantStatus: http.StatusBadRequest,
				because:    "exactly one request per body, with no room for a smuggled second object",
			},
			{
				name:       "empty body",
				body:       "",
				wantStatus: http.StatusBadRequest,
				because:    "an empty body is not a JSON object",
			},
			{
				name:       "not JSON at all",
				body:       `alpha`,
				wantStatus: http.StatusBadRequest,
				because:    "malformed JSON is a client error",
			},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec := postJSON(t, srv, httpapi.RouteEnroll, tc.body)
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d (%s); body %s", rec.Code, tc.wantStatus, tc.because, rec.Body.String())
				}
			})
		}
	})

	t.Run("an oversized body is refused before it is decoded", func(t *testing.T) {
		srv, _ := newAuthServer(t)
		_, _, pubB64 := newAuthKeypair(t)
		huge := `{"name":"` + strings.Repeat("a", 4*httpapi.MaxAuthRequestBytes) + `","public_key":"` + pubB64 + `","idempotency_key":"idem-1"}`

		rec := postJSON(t, srv, httpapi.RouteEnroll, huge)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413 for a body over MaxAuthRequestBytes (%d); body %s", rec.Code, httpapi.MaxAuthRequestBytes, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), strings.Repeat("a", 64)) {
			t.Error("the error body echoed the oversized input back")
		}
	})
}

// TestEnrollThenSessionNeverLogsACredential is a leak test, not a formality.
//
// The token is a live credential once the challenge completes; the signature
// and the public key are the material an operator has no need for. A log line
// carrying any of them turns log retention into credential retention, and the
// handlers are written to keep all three out — this asserts it stays that way.
func TestEnrollThenSessionNeverLogsACredential(t *testing.T) {
	srv, logBuf := newAuthServer(t)
	_, priv, pubB64 := newAuthKeypair(t)

	agentID := enrolOverHTTP(t, srv, "alpha", pubB64, "idem-1")

	beginRec := postJSON(t, srv, httpapi.RouteSessionBegin, `{"agent_id":"`+agentID+`"}`)
	if beginRec.Code != http.StatusOK {
		t.Fatalf("begin status = %d, want 200", beginRec.Code)
	}
	token := decodeBody(t, beginRec)["token"].(string)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(auth.SessionSigningContext+token)))

	if rec := postJSON(t, srv, httpapi.RouteSessionComplete, `{"token":"`+token+`","signature":"`+sig+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("complete status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	// A few failure paths too: an error path is the easiest place for an
	// untrusted value to be logged "for diagnosis".
	postJSON(t, srv, httpapi.RouteSessionComplete, `{"token":"`+token+`","signature":"`+base64.StdEncoding.EncodeToString(make([]byte, 8))+`"}`)
	postJSON(t, srv, httpapi.RouteEnroll, `{"name":"ALPHA","public_key":"`+pubB64+`","idempotency_key":"idem-2"}`)

	logs := logBuf.String()

	// Guard against a vacuous pass: the buffer must actually hold the records
	// these requests produced.
	if len(logLines(logBuf)) == 0 {
		t.Fatal("no log records captured, so this test proves nothing")
	}
	if !strings.Contains(logs, agentID) {
		t.Fatalf("the log does not mention %q, so it is not the log for these requests", agentID)
	}

	for _, secret := range []struct {
		what  string
		value string
	}{
		{"the session token", token},
		{"the base64 signature", sig},
		{"the base64 public key", pubB64},
	} {
		if strings.Contains(logs, secret.value) {
			t.Errorf("%s reached the server log", secret.what)
		}
	}
}

// TestEnrollRoutesAreAbsentWithoutAnAuthService pins the deliberate choice
// behind a nil Options.Auth: the three routes are NOT REGISTERED, so they 404
// like any other path this build does not serve. A route that exists and
// refuses would be a claim that the surface is there.
func TestEnrollRoutesAreAbsentWithoutAnAuthService(t *testing.T) {
	srv := httpapi.New(httpapi.Options{Identity: testIdentity(authTestBusID)})

	if srv.Auth() != nil {
		t.Fatalf("Auth() = %v, want nil when none was supplied", srv.Auth())
	}

	for _, path := range []string{httpapi.RouteEnroll, httpapi.RouteSessionBegin, httpapi.RouteSessionComplete} {
		rec := doRequest(t, srv, http.MethodPost, path, `{}`, "application/json")
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s status = %d, want 404 when the server was built without an auth service", path, rec.Code)
		}
	}

	t.Run("the rest of the surface is unaffected", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/healthz", "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET /healthz status = %d, want 200", rec.Code)
		}
	})
}
