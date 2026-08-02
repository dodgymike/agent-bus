package httpapi_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// msgTestBusID qualifies every agent id the messaging tests expect (invariant
// 2). It is distinct from authTestBusID so a cursor or a message id built by
// one suite can never accidentally satisfy the other.
const msgTestBusID = "bus-msg-test"

// newMessagingServer builds a Server with a REAL durable log, a REAL auth
// service and therefore a REAL hub.
//
// Nothing here is a double. These tests exist to prove the messaging surface a
// client actually meets — status codes, exact JSON field names, headers, the
// authentication boundary — and every one of those is produced by the
// interaction between the handler, the hub and the durable log. A stub for any
// of the three would let two of them disagree with no test noticing, which is
// the failure this file is meant to catch.
//
// The WAL lives under t.TempDir(): NEVER the tracked data/ dir.
func newMessagingServer(t *testing.T) (*httpapi.Server, *bytes.Buffer) {
	t.Helper()

	dir := t.TempDir()
	lg := &bytes.Buffer{}
	logger := logging.New(lg, logging.LevelDebug)

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
	svc, err := auth.NewService(auth.Options{Minter: minter})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}

	srv := httpapi.New(httpapi.Options{
		Identity: testIdentity(msgTestBusID),
		Logger:   logger,
		Durable:  walLog,
		Auth:     svc,
	})
	if srv.Hub() == nil {
		t.Fatal("the server built no hub from a real durable log, so no messaging route is registered; every assertion below would be about a 404")
	}
	return srv, lg
}

// testAgent is one enrolled agent with a live session, as a client holds it.
type testAgent struct {
	id    string
	token string
}

// enrolAndAuthenticate runs the full credential handshake and returns an agent
// holding a live bearer token, exactly as a real client would arrive.
func enrolAndAuthenticate(t *testing.T, srv *httpapi.Server, name string) testAgent {
	t.Helper()

	_, priv, pubB64 := newAuthKeypair(t)
	agentID := enrolOverHTTP(t, srv, name, pubB64, "idem-enrol-"+name)

	beginRec := postJSON(t, srv, httpapi.RouteSessionBegin, `{"agent_id":"`+agentID+`"}`)
	if beginRec.Code != http.StatusOK {
		t.Fatalf("session begin for %s = %d, want 200; body %s", name, beginRec.Code, beginRec.Body.String())
	}
	token, _ := decodeBody(t, beginRec)["token"].(string)
	if token == "" {
		t.Fatalf("session begin for %s returned no token", name)
	}

	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(auth.SessionSigningContext+token)))
	completeRec := postJSON(t, srv, httpapi.RouteSessionComplete, `{"token":"`+token+`","signature":"`+sig+`"}`)
	if completeRec.Code != http.StatusOK {
		t.Fatalf("session complete for %s = %d, want 200; body %s", name, completeRec.Code, completeRec.Body.String())
	}
	return testAgent{id: agentID, token: token}
}

// authed issues a request carrying a's bearer credential.
func authed(t *testing.T, srv *httpapi.Server, a testAgent, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// b64 spells a body the way a client sends it.
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// sendOK posts a broadcast or a DM and insists on the 201.
func sendOK(t *testing.T, srv *httpapi.Server, from testAgent, path, body string) map[string]interface{} {
	t.Helper()
	rec := authed(t, srv, from, http.MethodPost, path, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST %s = %d, want 201; body %s", path, rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)
}

// batchOf reads a batch response and returns the decoded body plus the message
// list, insisting on a 200.
func batchOf(t *testing.T, rec *httptest.ResponseRecorder) (map[string]interface{}, []interface{}) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("batch status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	msgs, _ := body["messages"].([]interface{})
	return body, msgs
}

// TestMessagingRoutesRequireAuthentication is the messaging half of invariant
// 3. It is deliberately separate from TestEveryRouteRequiresAuth, which walks a
// server built WITHOUT a durable log and therefore never sees these routes at
// all.
func TestMessagingRoutesRequireAuthentication(t *testing.T) {
	srv, _ := newMessagingServer(t)

	// probed counts the routes this loop actually ASSERTED on. The list below
	// is fixed, but a refactor that stops registering the messaging routes
	// would leave every one of them 404 rather than 401, and the guard makes
	// that visible rather than letting the loop pass on a technicality.
	var probed int
	registered := make(map[string]bool)
	for _, p := range srv.Routes() {
		registered[p] = true
	}

	for _, tc := range []struct {
		path   string
		method string
	}{
		{httpapi.RouteAgents, http.MethodGet},
		{httpapi.RouteBroadcast, http.MethodPost},
		{httpapi.RouteSend, http.MethodPost},
		{httpapi.RouteMessages, http.MethodGet},
		{httpapi.RouteWait, http.MethodGet},
	} {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			if !registered[tc.path] {
				t.Fatalf("%s is not a registered route on a server built with a durable log; the messaging surface is missing, not protected", tc.path)
			}
			if httpapi.IsUnauthenticatedRoute(tc.path) {
				t.Fatalf("%s is on the anonymous allow-list. No messaging route may be: it reads or writes another agent's traffic (invariant 3)", tc.path)
			}
			rec := doRequest(t, srv, tc.method, tc.path, "", "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with NO credential = %d, want 401", tc.method, tc.path, rec.Code)
			}
		})
		probed++
	}

	if probed == 0 {
		t.Fatal("this loop asserted nothing; the table is empty and the test proved nothing at all")
	}
}

// TestAgentsRoute covers GET /v1/agents (MSG-1) on the wire.
func TestAgentsRoute(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	rec := authed(t, srv, alpha, http.MethodGet, httpapi.RouteAgents, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	wantKeys(t, body, "agents", "count")

	agents, _ := body["agents"].([]interface{})
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2 (%s)", len(agents), rec.Body.String())
	}
	if got := body["count"]; got != float64(2) {
		t.Errorf("count = %v, want 2", got)
	}

	seen := map[string]bool{}
	for _, raw := range agents {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("agent entry is not an object: %v", raw)
		}
		wantKeys(t, entry, "agent_id", "name", "enrolled_at")
		id, _ := entry["agent_id"].(string)
		if _, _, _, err := ids.ParseAgentID(id); err != nil {
			t.Errorf("agent_id %q is not a fully-qualified id (invariant 2): %v", id, err)
		}
		if _, err := time.Parse(time.RFC3339Nano, entry["enrolled_at"].(string)); err != nil {
			t.Errorf("enrolled_at is not RFC3339: %v", err)
		}
		seen[id] = true
	}
	for _, want := range []string{alpha.id, beta.id} {
		if !seen[want] {
			t.Errorf("agent %q is missing from the roster listing", want)
		}
	}

	// No key material. A public key is public, but the agent list exists so
	// agents can address each other, not so every agent gets a copy of every
	// other agent's credential material.
	if strings.Contains(rec.Body.String(), "public_key") {
		t.Error("the agent list carries a public_key field; it must not")
	}
}

// TestBroadcastRoute covers POST /v1/broadcast (MSG-2) on the wire, including
// both halves of invariant 10 as a client sees them.
func TestBroadcastRoute(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	body := sendOK(t, srv, alpha, httpapi.RouteBroadcast,
		`{"body":"`+b64("hello bus")+`","idempotency_key":"bc-1"}`)
	wantKeys(t, body, "message_id", "seq", "from", "broadcast", "to", "sent_at", "content_sha256")

	msgID, _ := body["message_id"].(string)
	gotBus, gotSeq, err := ids.ParseMessageID(msgID)
	if err != nil {
		t.Fatalf("message_id %q is not a well-formed id: %v", msgID, err)
	}
	if gotBus != msgTestBusID {
		t.Errorf("message id bus half = %q, want %q", gotBus, msgTestBusID)
	}
	if body["seq"] != float64(gotSeq) {
		t.Errorf("seq = %v but the message id carries %d", body["seq"], gotSeq)
	}
	if gotSeq == 0 {
		t.Error("sequence 0 is never allocated")
	}
	if body["from"] != alpha.id {
		t.Errorf("from = %v, want the AUTHENTICATED sender %q; a client never chooses it", body["from"], alpha.id)
	}
	if body["broadcast"] != true {
		t.Errorf("broadcast = %v, want true", body["broadcast"])
	}

	t.Run("every other agent receives it and the sender does not", func(t *testing.T) {
		_, msgs := batchOf(t, authed(t, srv, beta, http.MethodGet, httpapi.RouteMessages, ""))
		if len(msgs) != 1 {
			t.Fatalf("beta sees %d messages, want 1", len(msgs))
		}
		m := msgs[0].(map[string]interface{})
		wantKeys(t, m, "message_id", "seq", "from", "broadcast", "to", "bus_path", "sent_at", "size", "content_sha256", "body")
		if m["message_id"] != msgID {
			t.Errorf("message_id = %v, want %q", m["message_id"], msgID)
		}
		if got := m["body"]; got != b64("hello bus") {
			t.Errorf("body = %v, want the base64 the sender posted", got)
		}
		if m["content_sha256"] != body["content_sha256"] {
			t.Errorf("content_sha256 on the read path (%v) differs from the send response (%v)", m["content_sha256"], body["content_sha256"])
		}

		_, own := batchOf(t, authed(t, srv, alpha, http.MethodGet, httpapi.RouteMessages, ""))
		if len(own) != 0 {
			t.Fatalf("the sender sees its own broadcast (%d messages); the contract excludes it", len(own))
		}
	})

	t.Run("a retry with the same key and the same payload replays the original", func(t *testing.T) {
		rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteBroadcast,
			`{"body":"`+b64("hello bus")+`","idempotency_key":"bc-1"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 on a legitimate retry; a retry must never be punished", rec.Code)
		}
		if got := rec.Header().Get(httpapi.IdempotencyReplayedHeader); got != "true" {
			t.Errorf("%s = %q, want \"true\"", httpapi.IdempotencyReplayedHeader, got)
		}
		if got := decodeBody(t, rec)["message_id"]; got != msgID {
			t.Errorf("message_id = %v on the retry, want the ORIGINAL %q", got, msgID)
		}
		// And nothing was re-applied.
		_, msgs := batchOf(t, authed(t, srv, beta, http.MethodGet, httpapi.RouteMessages, ""))
		if len(msgs) != 1 {
			t.Fatalf("beta sees %d messages after a retry, want 1: the retry was re-applied", len(msgs))
		}
	})

	t.Run("the same key with a DIFFERENT payload is a protocol violation", func(t *testing.T) {
		rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteBroadcast,
			`{"body":"`+b64("something else")+`","idempotency_key":"bc-1"}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		if got := rec.Header().Get("Connection"); !strings.EqualFold(got, "close") {
			t.Errorf("Connection = %q, want \"close\": invariant 10 disconnects a client that reuses a key for different content", got)
		}
	})

	t.Run("an idempotency key is required", func(t *testing.T) {
		rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteBroadcast, `{"body":"`+b64("x")+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 without an idempotency key (invariant 10)", rec.Code)
		}
	})
}

// TestSendRoute covers POST /v1/send (MSG-3) on the wire.
func TestSendRoute(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")
	gamma := enrolAndAuthenticate(t, srv, "gamma")

	body := sendOK(t, srv, alpha, httpapi.RouteSend,
		`{"to":"`+beta.id+`","body":"`+b64("just for you")+`","idempotency_key":"dm-1"}`)
	if body["broadcast"] != false {
		t.Errorf("broadcast = %v, want false", body["broadcast"])
	}
	to, _ := body["to"].([]interface{})
	if len(to) != 1 || to[0] != beta.id {
		t.Fatalf("to = %v, want exactly [%q]", to, beta.id)
	}

	t.Run("only the named recipient sees it", func(t *testing.T) {
		_, mine := batchOf(t, authed(t, srv, beta, http.MethodGet, httpapi.RouteMessages, ""))
		if len(mine) != 1 {
			t.Fatalf("the recipient sees %d messages, want 1", len(mine))
		}
		for _, other := range []testAgent{alpha, gamma} {
			_, msgs := batchOf(t, authed(t, srv, other, http.MethodGet, httpapi.RouteMessages, ""))
			if len(msgs) != 0 {
				t.Fatalf("agent %s sees %d messages of a DM addressed to somebody else", other.id, len(msgs))
			}
		}
	})

	t.Run("404 on an unknown recipient", func(t *testing.T) {
		unknown, err := ids.AgentID(msgTestBusID, "nosuch", 9)
		if err != nil {
			t.Fatalf("building a well-formed but unenrolled id: %v", err)
		}
		rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend,
			`{"to":"`+unknown+`","body":"`+b64("x")+`","idempotency_key":"dm-unknown"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 on a malformed recipient id", func(t *testing.T) {
		rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend,
			`{"to":"not-a-qualified-id","body":"`+b64("x")+`","idempotency_key":"dm-bad"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
		}
	})
}

// TestMessagesCursorRoute covers GET /v1/messages and the cursor contract
// (MSG-4) as a client meets them.
func TestMessagesCursorRoute(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	const total = 5
	for i := 0; i < total; i++ {
		sendOK(t, srv, alpha, httpapi.RouteBroadcast,
			`{"body":"`+b64(string(rune('a'+i)))+`","idempotency_key":"page-`+string(rune('a'+i))+`"}`)
	}

	// Page through with limit=2 and prove the cursor neither skips nor repeats.
	cursor := ""
	var got []string
	pages := 0
	for {
		path := httpapi.RouteMessages + "?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		body, msgs := batchOf(t, authed(t, srv, beta, http.MethodGet, path, ""))
		pages++
		if pages > total+2 {
			t.Fatalf("paging did not terminate after %d pages; the cursor is not advancing", pages)
		}
		for _, raw := range msgs {
			got = append(got, raw.(map[string]interface{})["message_id"].(string))
		}
		next, _ := body["cursor"].(string)
		if len(msgs) == 0 {
			if next != cursor {
				t.Fatalf("an EMPTY batch returned cursor %q, want the cursor that was sent (%q)", next, cursor)
			}
			if body["more"] != false {
				t.Errorf("more = %v on an empty batch, want false", body["more"])
			}
			break
		}
		if len(msgs) > 2 {
			t.Fatalf("limit=2 returned %d messages", len(msgs))
		}
		if next == cursor {
			t.Fatalf("a non-empty batch did not advance the cursor (%q)", cursor)
		}
		cursor = next
	}
	if len(got) != total {
		t.Fatalf("paging delivered %d messages, want %d (%v)", len(got), total, got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("message %s was delivered twice while paging", id)
		}
		seen[id] = true
	}

	t.Run("a cursor issued to another agent is refused", func(t *testing.T) {
		// Take BETA's live cursor and present it as ALPHA. The visibility
		// filter already uses the principal, so this cannot leak anything —
		// which is exactly why the check is worth having: it turns a silent
		// no-op into a rejected request an operator can see.
		rec := authed(t, srv, alpha, http.MethodGet, httpapi.RouteMessages+"?cursor="+cursor, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for a cursor bound to a different agent", rec.Code)
		}
	})

	t.Run("a malformed cursor is refused", func(t *testing.T) {
		for _, bad := range []string{"not-base64!!", base64.RawURLEncoding.EncodeToString([]byte("v1|only-two")), base64.RawURLEncoding.EncodeToString([]byte("v9|" + beta.id + "|1"))} {
			rec := authed(t, srv, beta, http.MethodGet, httpapi.RouteMessages+"?cursor="+bad, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("cursor %q gave %d, want 400", bad, rec.Code)
			}
		}
	})

	t.Run("limit is validated", func(t *testing.T) {
		for _, bad := range []string{"0", "-1", "abc", "100000"} {
			rec := authed(t, srv, beta, http.MethodGet, httpapi.RouteMessages+"?limit="+bad, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("limit=%s gave %d, want 400", bad, rec.Code)
			}
		}
	})
}

// TestWaitRoute covers GET /v1/wait (POLL-1/POLL-2) on the wire: the fast path,
// the wake, and the fact that a timeout is a 200 rather than an error.
func TestWaitRoute(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	t.Run("a timeout is 200 with an empty batch and the SAME cursor", func(t *testing.T) {
		start := hubStartCursor(t, srv, beta)
		began := time.Now()
		body, msgs := batchOf(t, authed(t, srv, beta, http.MethodGet, httpapi.RouteWait+"?timeout=1&cursor="+start, ""))
		if len(msgs) != 0 {
			t.Fatalf("a quiet bus delivered %d messages", len(msgs))
		}
		if body["timed_out"] != true {
			t.Errorf("timed_out = %v, want true", body["timed_out"])
		}
		if got := body["cursor"]; got != start {
			t.Errorf("cursor = %v after a timeout, want the cursor that was sent (%q)", got, start)
		}
		if elapsed := time.Since(began); elapsed < 500*time.Millisecond {
			t.Errorf("the long poll returned after %v; it did not park for its 1s deadline", elapsed)
		}
	})

	t.Run("an existing message returns immediately", func(t *testing.T) {
		sendOK(t, srv, alpha, httpapi.RouteBroadcast, `{"body":"`+b64("already here")+`","idempotency_key":"wait-1"}`)
		began := time.Now()
		_, msgs := batchOf(t, authed(t, srv, beta, http.MethodGet, httpapi.RouteWait+"?timeout=60", ""))
		if len(msgs) != 1 {
			t.Fatalf("the fast path returned %d messages, want 1", len(msgs))
		}
		if elapsed := time.Since(began); elapsed > 5*time.Second {
			t.Fatalf("the fast path took %v; it parked when it should not have", elapsed)
		}
	})

	t.Run("a parked poll is woken by a new broadcast", func(t *testing.T) {
		cursor := hubStartCursor(t, srv, beta)
		type result struct {
			msgs []interface{}
			err  error
		}
		done := make(chan result, 1)
		go func() {
			rec := authed(t, srv, beta, http.MethodGet, httpapi.RouteWait+"?timeout=30&cursor="+cursor, "")
			if rec.Code != http.StatusOK {
				done <- result{err: errStatus(rec.Code)}
				return
			}
			msgs, _ := decodeBody(t, rec)["messages"].([]interface{})
			done <- result{msgs: msgs}
		}()

		// Wait until the poll has actually PARKED before publishing, so the
		// test proves the wake rather than the fast path.
		waitForWaiters(t, srv, 1)
		sendOK(t, srv, alpha, httpapi.RouteBroadcast, `{"body":"`+b64("woken")+`","idempotency_key":"wait-wake"}`)

		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("the parked poll failed: %v", r.err)
			}
			if len(r.msgs) != 1 {
				t.Fatalf("the woken poll returned %d messages, want 1", len(r.msgs))
			}
			if got := r.msgs[0].(map[string]interface{})["body"]; got != b64("woken") {
				t.Errorf("body = %v, want the message that woke it", got)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the parked poll was never woken by a committed broadcast")
		}
	})

	t.Run("timeout is validated", func(t *testing.T) {
		for _, bad := range []string{"0", "-1", "abc", "100000"} {
			rec := authed(t, srv, beta, http.MethodGet, httpapi.RouteWait+"?timeout="+bad, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("timeout=%s gave %d, want 400", bad, rec.Code)
			}
		}
	})
}

// hubStartCursor returns a's CURRENT position, so a test can park a poll
// without first draining everything already on the bus.
func hubStartCursor(t *testing.T, srv *httpapi.Server, a testAgent) string {
	t.Helper()
	body, _ := batchOf(t, authed(t, srv, a, http.MethodGet, httpapi.RouteMessages+"?limit="+strconv.Itoa(hubMaxDrain), ""))
	c, _ := body["cursor"].(string)
	return c
}

// hubMaxDrain is a batch size large enough to drain anything these tests
// create in one call.
const hubMaxDrain = 256

// waitForWaiters blocks until the hub reports n parked long-polls, so a test
// never has to guess with a sleep whether a request has reached its select.
func waitForWaiters(t *testing.T, srv *httpapi.Server, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Hub().WaiterCount() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no long poll parked within the deadline (want %d, have %d)", n, srv.Hub().WaiterCount())
}

// errStatus renders an unexpected status as an error a goroutine can hand back.
type errStatus int

func (e errStatus) Error() string { return "unexpected status " + strconv.Itoa(int(e)) }
