package httpapi_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/hub"
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
	// ONE roster, shared by construction. The auth service writes it at
	// enrolment and the hub READS THROUGH to it (hub.RosterSource): there is no
	// second copy for a handler to forget to update, which is the whole of
	// AUTH-7. That is why an agent enrolled over HTTP below is immediately
	// addressable on /v1/send, with nothing in this package reporting it.
	roster := auth.NewMemoryRoster()
	svc, err := auth.NewService(auth.Options{Minter: minter, Roster: roster})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}

	// The hub is built by the CALLER now — httpapi no longer constructs one —
	// so this helper wires it the way cmd/agent-bus does: the same durable log,
	// a read-only replay pass over its file, and a live view of the roster.
	h, err := hub.Open(hub.Options{
		BusID:   msgTestBusID,
		DataDir: filepath.Dir(walLog.Path()),
		Durable: walLog,
		Replay: func(fn func(wal.Committed) error) (wal.Recovered, error) {
			return wal.Replay(walLog.Path(), fn)
		},
		NextIndex: walLog.Recovered().NextIndex,
		Roster:    authRosterView{roster},
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("opening the messaging hub: %v", err)
	}

	srv := httpapi.New(httpapi.Options{
		Identity: testIdentity(msgTestBusID),
		Logger:   logger,
		Durable:  walLog,
		Auth:     svc,
		Hub:      h,
	})
	if srv.Hub() == nil {
		t.Fatal("the server built no hub from a real durable log, so no messaging route is registered; every assertion below would be about a 404")
	}
	return srv, lg
}

// authRosterView adapts internal/auth's roster to the read-only view the hub
// serves from, exactly as cmd/agent-bus's hubRoster does for the real server.
//
// It is duplicated here rather than exported from cmd because the production
// adapter lives in package main and cannot be imported. Keep the two in step:
// the field that matters is EnrolledAt, which must be the agent's ORIGINAL
// enrolment instant — it is the epoch every read path filters with
// (store.Message.VisibleTo).
type authRosterView struct{ roster auth.Roster }

func (v authRosterView) Lookup(agentID string) (hub.Agent, bool) {
	e, ok := v.roster.Get(agentID)
	if !ok {
		return hub.Agent{}, false
	}
	return hub.Agent{AgentID: e.AgentID, Name: e.Name, EnrolledAt: e.EnrolledAt}, true
}

func (v authRosterView) List() []hub.Agent {
	entries := v.roster.List()
	out := make([]hub.Agent, 0, len(entries))
	for _, e := range entries {
		out = append(out, hub.Agent{AgentID: e.AgentID, Name: e.Name, EnrolledAt: e.EnrolledAt})
	}
	return out
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

// msgTestTimestampMs is the SENDER-clock reading every send below carries.
//
// It is a fixed value rather than time.Now() because it is COVERED BY THE
// SIGNATURE: a client computes its signature over this exact number, so a
// timestamp that moved between building the request and building the signature
// would be a bug a real client cannot have. Fixing it also keeps a response body
// byte-reproducible across runs.
const msgTestTimestampMs int64 = 1754130896789 // 2026-08-02T12:34:56.789Z

// msgTestSignature is a well-formed 64-byte placeholder, standard base64.
//
// THE BUS NEVER VERIFIES A MESSAGE SIGNATURE and must never be given the
// ability to: it does not hold the sender's messaging key and must not be
// trusted to police messages for senders it does not control. What it enforces
// is SHAPE — present, valid base64, exactly signing.SignatureSize bytes — so a
// real Ed25519 signature would prove nothing here that a constant does not.
//
// The negative cases (absent, not base64, 63 bytes, 65 bytes) belong to the
// SIGN-6 rejection suite and are deliberately NOT written here.
func msgTestSignature() string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, ed25519.SignatureSize))
}

// mintOverHTTP is STEP ONE of a send, performed the way a client performs it.
//
// SIGN-1 settled that the sender signs the ORIGIN bus's minted message id and
// sequence, which makes a send a two-step: reserve here, sign, then present the
// reservation back on /v1/send. A test that skipped this step would not be
// testing a shortcut, it would be testing a request no client can make — the
// send is refused with 409 because there is no reservation to spend.
func mintOverHTTP(t *testing.T, srv *httpapi.Server, from testAgent, op, key string) (messageID string, seq uint64) {
	t.Helper()
	rec := authed(t, srv, from, http.MethodPost, httpapi.RouteMint,
		`{"op":"`+op+`","idempotency_key":"`+key+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST %s = %d, want 201; body %s", httpapi.RouteMint, rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	messageID, _ = body["message_id"].(string)
	rawSeq, _ := body["seq"].(float64)
	if messageID == "" || rawSeq == 0 {
		t.Fatalf("%s returned message_id %q and seq %v; a reservation must carry both", httpapi.RouteMint, messageID, body["seq"])
	}
	return messageID, uint64(rawSeq)
}

// signedSendBody is the /v1/send request a client builds once it holds a
// reservation: the addressing, the payload, the reservation itself and the
// detached signature over them.
//
// `sender` is spelled out here rather than left off because SIGN-6 requires the
// request to name the sender it signed as, so the bus can refuse a message whose
// signed content contradicts the identity it authenticated. It is INPUT TO
// VALIDATE and never an identity — the bus takes the principal from the session.
func signedSendBody(to, payloadB64, key, sender, messageID string, seq uint64) string {
	return fmt.Sprintf(`{"to":%q,"body":%q,"idempotency_key":%q,"sender":%q,"message_id":%q,"seq":%d,"timestamp_ms":%d,"signature":%q}`,
		to, payloadB64, key, sender, messageID, seq, msgTestTimestampMs, msgTestSignature())
}

// sendDM runs the WHOLE two-step — mint, then send — and returns the raw
// recorder so a caller can assert on a status other than 201.
func sendDM(t *testing.T, srv *httpapi.Server, from testAgent, to, payloadB64, key string) *httptest.ResponseRecorder {
	t.Helper()
	msgID, seq := mintOverHTTP(t, srv, from, "send", key)
	return authed(t, srv, from, http.MethodPost, httpapi.RouteSend,
		signedSendBody(to, payloadB64, key, from.id, msgID, seq))
}

// sendOK is sendDM insisting on the 201.
func sendOK(t *testing.T, srv *httpapi.Server, from testAgent, to, payloadB64, key string) map[string]interface{} {
	t.Helper()
	rec := sendDM(t, srv, from, to, payloadB64, key)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST %s = %d, want 201; body %s", httpapi.RouteSend, rec.Code, rec.Body.String())
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
		{httpapi.RouteMint, http.MethodPost},
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

// TestMintRoute covers POST /v1/mint (SIGN-2) on the wire: the reservation a
// sender must hold before it can sign anything.
func TestMintRoute(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")

	rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteMint, `{"op":"send","idempotency_key":"mint-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	wantKeys(t, body, "message_id", "seq", "sender", "op", "expires_at")

	msgID, _ := body["message_id"].(string)
	gotBus, gotSeq, err := ids.ParseMessageID(msgID)
	if err != nil {
		t.Fatalf("message_id %q is not a well-formed id: %v", msgID, err)
	}
	if gotBus != msgTestBusID {
		t.Errorf("message id bus half = %q, want %q: the id a sender signs must name the ORIGIN bus (SIGN-1)", gotBus, msgTestBusID)
	}
	if body["seq"] != float64(gotSeq) {
		t.Errorf("seq = %v but the message id carries %d; the canonical format encodes both and checks they agree", body["seq"], gotSeq)
	}
	if gotSeq == 0 {
		t.Error("sequence 0 is never allocated")
	}
	// The sender is the AUTHENTICATED principal echoed back. There is no sender
	// field in the request and there never may be: a reservation minted under a
	// name of the caller's choosing is a signed message id attributable to
	// somebody else (invariant 1).
	if body["sender"] != alpha.id {
		t.Errorf("sender = %v, want the authenticated principal %q", body["sender"], alpha.id)
	}
	if body["op"] != "send" {
		t.Errorf("op = %v, want \"send\"", body["op"])
	}
	if _, err := time.Parse(time.RFC3339Nano, body["expires_at"].(string)); err != nil {
		t.Errorf("expires_at is not RFC3339: %v", err)
	}

	t.Run("a re-mint under the same key returns the SAME reservation and allocates nothing", func(t *testing.T) {
		again := authed(t, srv, alpha, http.MethodPost, httpapi.RouteMint, `{"op":"send","idempotency_key":"mint-1"}`)
		if again.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 on a legitimate re-mint; a retry must never be punished (invariant 10)", again.Code)
		}
		if got := again.Header().Get(httpapi.IdempotencyReplayedHeader); got != "true" {
			t.Errorf("%s = %q, want \"true\"", httpapi.IdempotencyReplayedHeader, got)
		}
		// BYTE-IDENTICAL, expires_at included. A re-mint that returned a fresh
		// expiry would let a client hold a reservation open for ever by asking
		// again, and would make the response body disagree with the first one a
		// client may already have signed against.
		if again.Body.String() != rec.Body.String() {
			t.Errorf("the re-mint body is\n  %s\nwant the ORIGINAL, byte for byte\n  %s", again.Body.String(), rec.Body.String())
		}
	})

	t.Run("op is validated", func(t *testing.T) {
		for _, bad := range []string{`{"op":"relay","idempotency_key":"mint-bad-op"}`, `{"idempotency_key":"mint-no-op"}`} {
			r := authed(t, srv, alpha, http.MethodPost, httpapi.RouteMint, bad)
			if r.Code != http.StatusBadRequest {
				t.Fatalf("%s gave %d, want 400: an unrecognised op must be refused, never defaulted", bad, r.Code)
			}
		}
	})

	t.Run("an idempotency key is required", func(t *testing.T) {
		r := authed(t, srv, alpha, http.MethodPost, httpapi.RouteMint, `{"op":"send"}`)
		if r.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 without an idempotency key (invariant 10)", r.Code)
		}
	})
}

// TestBroadcastRouteIsRefused pins the 501 (SIGN-6).
//
// This test USED to be TestBroadcastRoute and used to assert a working
// broadcast: the fan-out, the read-back and both halves of invariant 10. The
// route now fails CLOSED, so what is left to prove is that it refuses, that it
// says WHY, and that it writes nothing on the way.
//
// The coverage that moved rather than vanished: the wire-level invariant 10
// assertions — legitimate retry replays, same key with different content is a
// 409 that disconnects, a key is required — now live in TestSendRoute. They are
// properties of publish, which both routes share, so they are proved on the
// route that still reaches it.
//
// The reason for the refusal is worth restating because deleting this test is
// the obvious way to "fix" it: signing.Canonicalize rejects an empty recipient
// set and store.Message records a broadcast as a FLAG rather than an expanded
// roster snapshot, so a broadcast has no canonical audience under signing format
// v1. SIGN-6 admits no unsigned message type, so the alternative to refusing is
// leaving one route on the bus that accepts unsigned traffic — the easiest hole
// in the whole system to find.
func TestBroadcastRouteIsRefused(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteBroadcast,
		`{"body":"`+b64("hello bus")+`","idempotency_key":"bc-1"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("POST %s = %d, want 501; body %s", httpapi.RouteBroadcast, rec.Code, rec.Body.String())
	}

	// The refusal must EXPLAIN itself. A bare 501 tells a client the route is
	// missing; this one has to say that the message could not be signed, or the
	// client's only rational response is to retry for ever.
	msg, _ := decodeBody(t, rec)["error"].(string)
	for _, want := range []string{"signed", "recipient set", "unsigned"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the 501 body does not mention %q, so a client cannot tell why a broadcast is refused: %q", want, msg)
		}
	}

	// NOT dressed up as transient. A Retry-After would put a well-behaved client
	// into a loop that can never succeed.
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q on a 501; the refusal is permanent under signing format v1, not a transient one to retry", got)
	}

	// And NOTHING was written. The refusal happens before the body is decoded,
	// so no sequence is burned, no WAL record is made and no agent receives
	// anything.
	if _, msgs := batchOf(t, authed(t, srv, beta, http.MethodGet, httpapi.RouteMessages, "")); len(msgs) != 0 {
		t.Fatalf("beta received %d messages from a REFUSED broadcast; a 501 must write nothing", len(msgs))
	}

	t.Run("an anonymous caller is still 401, not 501", func(t *testing.T) {
		// Authentication runs FIRST. A route that told an unauthenticated caller
		// what it does and does not implement would describe the messaging
		// surface to somebody with no business knowing it exists.
		r := doRequest(t, srv, http.MethodPost, httpapi.RouteBroadcast, `{"body":"`+b64("x")+`","idempotency_key":"k"}`, "")
		if r.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 with no credential", r.Code)
		}
	})
}

// TestSendRoute covers POST /v1/send (MSG-3, SIGN-6) on the wire, including both
// halves of invariant 10 as a client sees them.
func TestSendRoute(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")
	gamma := enrolAndAuthenticate(t, srv, "gamma")

	body := sendOK(t, srv, alpha, beta.id, b64("just for you"), "dm-1")
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
	if body["from"] != alpha.id {
		t.Errorf("from = %v, want the AUTHENTICATED sender %q; a client never chooses it", body["from"], alpha.id)
	}
	if body["broadcast"] != false {
		t.Errorf("broadcast = %v, want false", body["broadcast"])
	}
	to, _ := body["to"].([]interface{})
	if len(to) != 1 || to[0] != beta.id {
		t.Fatalf("to = %v, want exactly [%q]", to, beta.id)
	}

	t.Run("only the named recipient sees it, and the signature reaches it", func(t *testing.T) {
		_, mine := batchOf(t, authed(t, srv, beta, http.MethodGet, httpapi.RouteMessages, ""))
		if len(mine) != 1 {
			t.Fatalf("the recipient sees %d messages, want 1", len(mine))
		}
		m := mine[0].(map[string]interface{})
		wantKeys(t, m, "message_id", "seq", "from", "broadcast", "to", "bus_path", "sent_at", "size", "content_sha256", "timestamp_ms", "signature", "body")
		if m["message_id"] != msgID {
			t.Errorf("message_id = %v, want %q", m["message_id"], msgID)
		}
		if got := m["body"]; got != b64("just for you") {
			t.Errorf("body = %v, want the base64 the sender posted", got)
		}
		if m["content_sha256"] != body["content_sha256"] {
			t.Errorf("content_sha256 on the read path (%v) differs from the send response (%v)", m["content_sha256"], body["content_sha256"])
		}
		// The recipient cannot verify without these two, and they must be the
		// SENDER's, carried through untouched.
		if m["signature"] != msgTestSignature() {
			t.Errorf("signature = %v, want the sender's, carried through unaltered", m["signature"])
		}
		if m["timestamp_ms"] != float64(msgTestTimestampMs) {
			t.Errorf("timestamp_ms = %v, want the SENDER's clock %d; the bus's own clock is sent_at and is NOT covered by the signature", m["timestamp_ms"], msgTestTimestampMs)
		}
		if m["sent_at"] == m["timestamp_ms"] {
			t.Error("sent_at and timestamp_ms are the same value; they are two different facts told by two different parties and only one is signed")
		}

		for _, other := range []testAgent{alpha, gamma} {
			_, msgs := batchOf(t, authed(t, srv, other, http.MethodGet, httpapi.RouteMessages, ""))
			if len(msgs) != 0 {
				t.Fatalf("agent %s sees %d messages of a DM addressed to somebody else", other.id, len(msgs))
			}
		}
	})

	t.Run("a retry with the same key and the same payload replays the original", func(t *testing.T) {
		// The whole two-step is retried, mint included: a re-mint under a held
		// key returns the SAME assignment, so the client re-presents the SAME
		// signature and the send is answered from the applied-key table.
		rec := sendDM(t, srv, alpha, beta.id, b64("just for you"), "dm-1")
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 on a legitimate retry; a retry must never be punished; body %s", rec.Code, rec.Body.String())
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

	// NARROWED 2026-08-07: this was asserted to disconnect. It no longer does.
	// The key is the caller's OWN — keys are scoped per agent — so this is a
	// client that lost track of its keys, and dropping its socket would kill the
	// long poll and every other request it had pipelined there. The disconnect
	// moved to checkSignedMint's sender-mismatch 403, which is where a THIRD
	// PARTY's signed message presents. disconnect_socket_test.go proves both at
	// the socket; this case only pins the status and the absent header.
	t.Run("the same key with a DIFFERENT payload is a protocol violation, rejected without disconnecting", func(t *testing.T) {
		rec := sendDM(t, srv, alpha, beta.id, b64("something else"), "dm-1")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Connection"); strings.EqualFold(got, "close") {
			t.Errorf("Connection = %q, want it absent: a client reusing its OWN key is confused, not hostile, and is not disconnected", got)
		}
	})

	t.Run("an idempotency key is required", func(t *testing.T) {
		// Sent WITHOUT a mint on purpose: there is no key to mint under, so the
		// key check is what must answer, and it must answer 400 rather than the
		// 409 a missing reservation would produce.
		rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend,
			signedSendBody(beta.id, b64("x"), "", alpha.id, msgID, gotSeq))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 without an idempotency key (invariant 10); body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("404 on an unknown recipient", func(t *testing.T) {
		unknown, err := ids.AgentID(msgTestBusID, "nosuch", 9)
		if err != nil {
			t.Fatalf("building a well-formed but unenrolled id: %v", err)
		}
		rec := sendDM(t, srv, alpha, unknown, b64("x"), "dm-unknown")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 on a malformed recipient id", func(t *testing.T) {
		rec := sendDM(t, srv, alpha, "not-a-qualified-id", b64("x"), "dm-bad")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("409 when no reservation was minted for the key", func(t *testing.T) {
		// The send is well formed in every other respect and names an id this
		// bus really did mint — for a DIFFERENT key. That is exactly the state a
		// client is in after a restart, and the response has to point at the
		// remedy rather than look like a transient failure.
		rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend,
			signedSendBody(beta.id, b64("x"), "dm-never-minted", alpha.id, msgID, gotSeq))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
		}
		if msg, _ := decodeBody(t, rec)["error"].(string); !strings.Contains(msg, httpapi.RouteMint) {
			t.Errorf("the 409 body does not name %s, so a client cannot tell what to do next: %q", httpapi.RouteMint, msg)
		}
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Errorf("Retry-After = %q; replaying this exact request can never succeed, so it must not be dressed up as transient", got)
		}
	})
}

// TestMessagesCursorRoute covers GET /v1/messages and the cursor contract
// (MSG-4) as a client meets them.
func TestMessagesCursorRoute(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	// Directed sends rather than broadcasts: /v1/broadcast is refused under
	// signing format v1 (see TestBroadcastRouteIsRefused), and the cursor
	// contract is about the READ path, which does not care how a message was
	// addressed.
	const total = 5
	for i := 0; i < total; i++ {
		sendOK(t, srv, alpha, beta.id, b64(string(rune('a'+i))), "page-"+string(rune('a'+i)))
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
		// NOTE: an unknown cursor VERSION is deliberately NOT in this table any
		// more. Since SIGN-1-FU-REORDER-WATERMARK it is accepted and remapped to
		// position 0 rather than refused — see the two subtests below, which
		// replace the entry that used to live here. Malformed SHAPE is still 400.
		for _, bad := range []string{"not-base64!!", base64.RawURLEncoding.EncodeToString([]byte("v1|only-two"))} {
			rec := authed(t, srv, beta, http.MethodGet, httpapi.RouteMessages+"?cursor="+bad, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("cursor %q gave %d, want 400", bad, rec.Code)
			}
		}
	})

	// An UNKNOWN cursor version is accepted and remapped to the start of the
	// retained window. Rejecting it would be unrecoverable rather than merely
	// lossy: a 400 is not retried by the watch loop and nothing clears the
	// stored cursor, so the same value would be re-presented on every poll for
	// ever. One replay is the correct price; at-least-once delivery already
	// requires every client to tolerate duplicates.
	t.Run("an unknown cursor version is remapped, not refused", func(t *testing.T) {
		c := base64.RawURLEncoding.EncodeToString([]byte("v9|" + beta.id + "|1"))
		rec := authed(t, srv, beta, http.MethodGet, httpapi.RouteMessages+"?cursor="+c, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("an unknown-version cursor gave %d, want 200 with a remap to position 0", rec.Code)
		}
	})

	// THE ORDERING THAT MAKES THE REMAP SAFE, asserted at the wire. The agent
	// binding is checked BEFORE the version, so an old-version cursor issued to
	// somebody else is still refused. An early return on the version branch
	// would bypass the binding check and is invisible to the subtest above —
	// this is the only route-level guard against that.
	t.Run("an unknown version bound to another agent is still refused", func(t *testing.T) {
		c := base64.RawURLEncoding.EncodeToString([]byte("v9|" + alpha.id + "|1"))
		rec := authed(t, srv, beta, http.MethodGet, httpapi.RouteMessages+"?cursor="+c, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("an unknown-version cursor bound to another agent gave %d, want 400", rec.Code)
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
		sendOK(t, srv, alpha, beta.id, b64("already here"), "wait-1")
		began := time.Now()
		_, msgs := batchOf(t, authed(t, srv, beta, http.MethodGet, httpapi.RouteWait+"?timeout=60", ""))
		if len(msgs) != 1 {
			t.Fatalf("the fast path returned %d messages, want 1", len(msgs))
		}
		if elapsed := time.Since(began); elapsed > 5*time.Second {
			t.Fatalf("the fast path took %v; it parked when it should not have", elapsed)
		}
	})

	t.Run("a parked poll is woken by a new message", func(t *testing.T) {
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
		sendOK(t, srv, alpha, beta.id, b64("woken"), "wait-wake")

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
			t.Fatal("the parked poll was never woken by a committed message")
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
