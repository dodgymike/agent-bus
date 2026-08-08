package httpapi_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/httpapi"
)

// INVARIANT 10's DISCONNECT, PROVED AT THE SOCKET.
//
// # Why none of this may be tested through a status code or a header
//
// The disconnect is not something the response body says; it is something that
// happens to the TCP connection AFTER the response is written. Two ways of
// "testing" it are both worthless, and both have already produced a wrong
// finding about this exact code:
//
//   - Asserting the `Connection: close` HEADER. That proves the handler set a
//     header, not that net/http acted on it. The header and the behaviour are
//     two facts and only the second one is the contract.
//   - Issuing a follow-up request through an http.Client. Every pooled
//     transport TRANSPARENTLY REDIALS a closed connection, so a 200 on the
//     second request is equally consistent with "the socket stayed open" and
//     "the socket was closed and silently replaced".
//
// So every case below PINS ONE net.Conn, speaks HTTP/1.1 on it by hand, and
// then asks the socket itself. A raw net.Conn cannot redial: if a follow-up
// request on it succeeds, that connection was genuinely still there.
//
// # The policy these tests pin (narrowed 2026-08-07)
//
// The disconnect aims at the party that presented ANOTHER AGENT'S signed
// message, and at nobody else:
//
//   - third-party replay — `sender` names an agent that is not the
//     authenticated caller: 403 AND DISCONNECT.
//   - a client's own idempotency key reused with a different payload: 409,
//     rejected and logged, CONNECTION KEPT. It is the confused-but-honest case,
//     and disconnecting it kills every other in-flight request that client had
//     on the connection — an abuse defence aimed at the wrong party.
//   - a client re-presenting its OWN spent reservation under a fresh key: 409,
//     CONNECTION KEPT. Same reason.
//
// TestCrossMintIsIndistinguishableFromAnHonestSpentReservation below records,
// as executable evidence, why the fourth case an operator asked for is NOT
// implemented here.

// rawResponse is one response read off a pinned connection, captured whole
// because the connection is reused and the body cannot be read twice.
type rawResponse struct {
	status int
	header http.Header
	body   string
}

// pinnedConn is ONE HTTP/1.1 connection to a real listener, driven by hand.
//
// It exists so a test can ask "is this socket still there" and get an answer
// about THIS socket. http.Client cannot answer that question about any socket,
// because its whole job is to hide which one it used.
type pinnedConn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	host string
}

// dialPinned opens one connection to ts and keeps it.
func dialPinned(t *testing.T, ts *httptest.Server) *pinnedConn {
	t.Helper()
	host := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		t.Fatalf("dialing the test bus at %s: %v", host, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &pinnedConn{t: t, conn: conn, br: bufio.NewReader(conn), host: host}
}

// do writes one request on the pinned connection and reads its response to
// completion, so the connection is left at a message boundary and any close the
// server performs is attributable to the response just read.
func (p *pinnedConn) do(method, path, token, body string) rawResponse {
	p.t.Helper()

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s HTTP/1.1\r\n", method, path)
	fmt.Fprintf(&sb, "Host: %s\r\n", p.host)
	if token != "" {
		fmt.Fprintf(&sb, "Authorization: Bearer %s\r\n", token)
	}
	if body != "" {
		fmt.Fprintf(&sb, "Content-Type: application/json\r\n")
	}
	// ALWAYS sent, even for an empty body: a request with no Content-Length and
	// no body is legal, but spelling it out keeps the framing unambiguous on a
	// connection this test intends to reuse.
	fmt.Fprintf(&sb, "Content-Length: %d\r\n", len(body))
	fmt.Fprintf(&sb, "\r\n%s", body)

	if err := p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		p.t.Fatalf("setting a write deadline: %v", err)
	}
	if _, err := io.WriteString(p.conn, sb.String()); err != nil {
		p.t.Fatalf("writing %s %s on the pinned connection: %v", method, path, err)
	}

	if err := p.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		p.t.Fatalf("setting a read deadline: %v", err)
	}
	resp, err := http.ReadResponse(p.br, &http.Request{Method: method})
	if err != nil {
		p.t.Fatalf("reading the response to %s %s on the pinned connection: %v", method, path, err)
	}
	// Read the body to EOF, so the next byte on the wire is either a new
	// response or the FIN — never a leftover from this one.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		p.t.Fatalf("draining the response body of %s %s: %v", method, path, err)
	}
	_ = resp.Body.Close()
	return rawResponse{status: resp.StatusCode, header: resp.Header, body: string(raw)}
}

// socketProbeTimeout is how long socketClosed waits before concluding the
// connection is still open.
//
// Every one of these tests runs against a loopback listener in the same
// process, so a FIN is already in the receive buffer by the time the response
// has been read — the wait only ever elapses in full for the OPEN cases, where
// it is pure cost. 500ms is ~1000x the loopback round trip and still bounds a
// suite that would otherwise spend 2s per negative assertion.
const socketProbeTimeout = 500 * time.Millisecond

// socketClosed reports whether the SERVER closed this connection.
//
// It reads with a deadline. A closed connection yields io.EOF (or a reset)
// immediately; a live idle keep-alive connection yields nothing at all and the
// read times out, which is the discriminator. The deadline is the only cost of
// proving a NEGATIVE here, and there is no cheaper honest way to prove one.
//
// bufio.Reader.Peek CONSUMES the stored error, so a timed-out probe does not
// poison the follow-up ReadResponse that assertStaysOpen performs next.
func (p *pinnedConn) socketClosed() bool {
	p.t.Helper()
	if err := p.conn.SetReadDeadline(time.Now().Add(socketProbeTimeout)); err != nil {
		p.t.Fatalf("setting a read deadline: %v", err)
	}
	_, err := p.br.Peek(1)
	switch {
	case err == nil:
		// Unread bytes on an idle connection: the previous response was not
		// fully drained, and every conclusion drawn from this socket would be
		// about the wrong message. Fail loudly rather than guess.
		p.t.Fatalf("the pinned connection has unread bytes at what should be a message boundary; the previous response was not fully drained and this probe would be meaningless")
		return false
	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed), isConnReset(err):
		return true
	case os.IsTimeout(err):
		return false
	default:
		p.t.Fatalf("probing the pinned connection: unexpected error %v", err)
		return false
	}
}

func isConnReset(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection reset by peer")
}

// assertStaysOpen proves the connection survived, by USING it.
//
// A follow-up request on a raw net.Conn cannot be silently redialled — that is
// the whole reason these tests do not use http.Client — so a 200 here is proof
// this exact socket was still carrying traffic after the rejection.
func (p *pinnedConn) assertStaysOpen(token, why string) {
	p.t.Helper()
	if p.socketClosed() {
		p.t.Fatalf("the server CLOSED the pinned connection: %s", why)
	}
	resp := p.do(http.MethodGet, httpapi.RouteAgents, token, "")
	if resp.status != http.StatusOK {
		p.t.Fatalf("the follow-up request on the same pinned connection = %d, want 200: %s", resp.status, why)
	}
}

func (p *pinnedConn) assertClosed(why string) {
	p.t.Helper()
	if !p.socketClosed() {
		p.t.Fatalf("the server left the pinned connection OPEN: %s", why)
	}
}

// warmUp issues one ordinary request and insists it succeeds, so that a
// "closed" verdict later cannot be confused with a connection that never
// carried a request in the first place.
func (p *pinnedConn) warmUp(token string) {
	p.t.Helper()
	if resp := p.do(http.MethodGet, httpapi.RouteAgents, token, ""); resp.status != http.StatusOK {
		p.t.Fatalf("the pinned connection = %d on an ordinary request, want 200; the socket evidence that follows would be meaningless", resp.status)
	}
}

// socketBus is newMessagingServer behind a real listener.
func socketBus(t *testing.T) (*httpapi.Server, *httptest.Server) {
	t.Helper()
	srv, _ := newMessagingServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts
}

// errorOf reads the "error" string out of a captured JSON error body.
func errorOf(t *testing.T, raw string) string {
	t.Helper()
	var v map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decoding an error body %q: %v", raw, err)
	}
	s, _ := v["error"].(string)
	if s == "" {
		t.Fatalf("the error body %q carries no \"error\" string", raw)
	}
	return s
}

// TestThirdPartyReplayDisconnects is CASE 1: an agent presents a message signed
// by SOMEBODY ELSE, verbatim, over its own authenticated session.
//
// This is invariant 10's replay clause and the one party the disconnect is
// actually for. A signature does not stop replay — a valid signed message can be
// resent byte for byte — so the bus refusing it is the only thing that does.
func TestThirdPartyReplayDisconnects(t *testing.T) {
	srv, ts := socketBus(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")
	mallory := enrolAndAuthenticate(t, srv, "mallory")

	// Alpha sends a real message, so what mallory replays below is an
	// ALREADY-ACCEPTED signed message and not a fabrication.
	msgID, seq := mintOverHTTP(t, srv, alpha, "send", "replay-victim")
	victimBody := signedSendBody(beta.id, b64("alpha's own words"), "replay-victim", alpha.id, msgID, seq)
	if rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend, victimBody); rec.Code != http.StatusCreated {
		t.Fatalf("alpha's genuine send = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	p := dialPinned(t, ts)
	p.warmUp(mallory.token)

	resp := p.do(http.MethodPost, httpapi.RouteSend, mallory.token, victimBody)
	if resp.status != http.StatusForbidden {
		t.Fatalf("replaying alpha's signed message as mallory = %d, want 403; body %s", resp.status, resp.body)
	}
	p.assertClosed("a third party replayed another agent's signed message, which invariant 10 answers with a disconnect")

	// AND THE REFUSAL COST NOTHING DURABLE. The check runs before hub.Send, so
	// the claim "no idempotency key consumed, no WAL record, nothing delivered"
	// is structural — but it is asserted rather than asserted-in-a-comment,
	// because the day somebody moves this check into the hub it would silently
	// stop holding.
	if msgs := visibleMessages(t, srv, beta); len(msgs) != 1 {
		t.Fatalf("beta sees %d messages after a REFUSED replay, want 1 (alpha's original only): the replay was delivered", len(msgs))
	}
	// The key mallory replayed is still usable by its rightful owner, which is
	// what "consumed no idempotency key" means observably.
	if rec := authed(t, srv, mallory, http.MethodPost, httpapi.RouteMint,
		`{"op":"send","idempotency_key":"replay-victim"}`); rec.Code != http.StatusCreated {
		t.Fatalf("minting under the key the refused replay carried = %d, want 201; the rejection consumed the key", rec.Code)
	}
}

// TestMalformedSenderDoesNotDisconnect is the guard on the DISCONNECT'S GATE,
// and it is the reason that gate exists.
//
// The first draft of this change disconnected on ANY sender mismatch, and a
// reviewer measured three shapes an HONEST single-identity client reaches by
// accident: the field omitted, the bus prefix dropped, and a trailing space.
// All three closed the socket. That is exactly the "abuse defence aimed at the
// wrong party" pattern the whole task exists to undo, reproduced one level down
// — so the disconnect now fires only when the claim PARSES as a fully-qualified
// agent id, i.e. when it actually names somebody.
//
// The status stays 403 in every case; only the disconnect is gated.
func TestMalformedSenderDoesNotDisconnect(t *testing.T) {
	srv, ts := socketBus(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	for _, tc := range []struct{ name, key, sender string }{
		{"the sender field is omitted entirely", "malformed-1", ""},
		{"an UNQUALIFIED id, the bus prefix dropped (invariant 2)", "malformed-2", "alpha-1"},
		{"a trailing space", "malformed-3", alpha.id + " "},
		{"a leading space", "malformed-4", " " + alpha.id},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgID, seq := mintOverHTTP(t, srv, alpha, "send", tc.key)
			p := dialPinned(t, ts)
			p.warmUp(alpha.token)

			resp := p.do(http.MethodPost, httpapi.RouteSend, alpha.token,
				signedSendBody(beta.id, b64("honest but mis-filled"), tc.key, tc.sender, msgID, seq))
			if resp.status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %s", resp.status, resp.body)
			}
			p.assertStaysOpen(alpha.token, "a malformed `sender` names NOBODY: it is a client that failed to fill the field, not one replaying another agent's message")
		})
	}
}

// TestSameAgentKeyReuseKeepsTheConnection is CASE 3, and it is a REVERSAL: this
// path disconnected until 2026-08-07 and must not again.
//
// Same key + different payload is a protocol violation and is refused. It is
// also, overwhelmingly, a BUG IN AN HONEST CLIENT — the caller is authenticated,
// it is reusing ITS OWN key, and nothing here involves another agent's material.
// Killing its connection destroys every unrelated in-flight request that client
// had on it, which punishes the party most likely to be honest.
func TestSameAgentKeyReuseKeepsTheConnection(t *testing.T) {
	srv, ts := socketBus(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	const key = "reuse-1"
	msgID, seq := mintOverHTTP(t, srv, alpha, "send", key)
	if rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend,
		signedSendBody(beta.id, b64("first payload"), key, alpha.id, msgID, seq)); rec.Code != http.StatusCreated {
		t.Fatalf("the first send = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	p := dialPinned(t, ts)
	p.warmUp(alpha.token)

	// The SAME key, a DIFFERENT payload. The reservation is the one already
	// minted under that key, so the ONLY thing that differs is the payload.
	resp := p.do(http.MethodPost, httpapi.RouteSend, alpha.token,
		signedSendBody(beta.id, b64("a DIFFERENT payload"), key, alpha.id, msgID, seq))
	if resp.status != http.StatusConflict {
		t.Fatalf("reusing a key with a different payload = %d, want 409; body %s", resp.status, resp.body)
	}
	if got := resp.header.Get("Connection"); strings.EqualFold(got, "close") {
		t.Errorf("the 409 carried Connection: %q; this path is reject-and-log, not disconnect", got)
	}
	p.assertStaysOpen(alpha.token, "an agent reused its OWN idempotency key with a different payload, which is rejected and logged but never disconnected")
}

// TestEnrolKeyReuseKeepsTheConnection is CASE 3 on the OTHER route that carried
// the same disconnect, /v1/enroll.
//
// It is worth its own case because /v1/enroll is UNAUTHENTICATED: the party
// whose connection would be dropped is one that has not proved anything yet, so
// dropping it is even less targeted than on /v1/send, and the honest client it
// hits is one part-way through obtaining a credential.
func TestEnrolKeyReuseKeepsTheConnection(t *testing.T) {
	srv, ts := socketBus(t)
	_, _, pubB64 := newAuthKeypair(t)
	_, _, otherB64 := newAuthKeypair(t)

	if rec := postJSON(t, srv, httpapi.RouteEnroll,
		`{"name":"alpha","public_key":"`+pubB64+`","idempotency_key":"enrol-reuse"}`); rec.Code != http.StatusCreated {
		t.Fatalf("the first enrolment = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	p := dialPinned(t, ts)
	// No token: /v1/enroll is unauthenticated, and so is /healthz, which is what
	// this connection is warmed up and re-probed with.
	if resp := p.do(http.MethodGet, "/healthz", "", ""); resp.status != http.StatusOK {
		t.Fatalf("the pinned connection = %d on /healthz, want 200", resp.status)
	}

	resp := p.do(http.MethodPost, httpapi.RouteEnroll, "",
		`{"name":"alpha","public_key":"`+otherB64+`","idempotency_key":"enrol-reuse"}`)
	if resp.status != http.StatusConflict {
		t.Fatalf("reusing an enrolment key with different key material = %d, want 409; body %s", resp.status, resp.body)
	}
	if got := resp.header.Get("Connection"); strings.EqualFold(got, "close") {
		t.Errorf("the 409 carried Connection: %q; this path is reject-and-log, not disconnect", got)
	}
	if p.socketClosed() {
		t.Fatal("the server CLOSED the connection on an enrolment key-reuse conflict; that path is reject-and-log")
	}
	if resp := p.do(http.MethodGet, "/healthz", "", ""); resp.status != http.StatusOK {
		t.Fatalf("the follow-up request on the same pinned connection = %d, want 200", resp.status)
	}
}

// TestSpentOwnReservationKeepsTheConnection is CASE 4 — THE TRAP TEST.
//
// It exists because 409 "no matching sequence reservation" is reached by TWO
// different actors and only one of them is hostile. Here it is the honest one:
// alpha spends a reservation on a real send, then re-presents that same, now
// spent, message id under a FRESH idempotency key. That is a confused client,
// not theft, and a blanket "disconnect on this 409" would repeat the exact
// mistake this change is undoing, one level down.
//
// If this test ever goes red because somebody added a disconnect to the 409,
// read TestCrossMintIsIndistinguishableFromAnHonestSpentReservation next: it
// proves the server cannot tell that somebody's intended target from this one.
func TestSpentOwnReservationKeepsTheConnection(t *testing.T) {
	srv, ts := socketBus(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	msgID, seq := mintOverHTTP(t, srv, alpha, "send", "spend-1")
	if rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend,
		signedSendBody(beta.id, b64("spent it"), "spend-1", alpha.id, msgID, seq)); rec.Code != http.StatusCreated {
		t.Fatalf("the first send = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	p := dialPinned(t, ts)
	p.warmUp(alpha.token)

	// A FRESH key, alpha's OWN already-spent reservation. Nothing here belongs
	// to another agent.
	resp := p.do(http.MethodPost, httpapi.RouteSend, alpha.token,
		signedSendBody(beta.id, b64("again please"), "spend-2-fresh-key", alpha.id, msgID, seq))
	if resp.status != http.StatusConflict {
		t.Fatalf("re-presenting an own spent reservation under a fresh key = %d, want 409; body %s", resp.status, resp.body)
	}
	if got := resp.header.Get("Connection"); strings.EqualFold(got, "close") {
		t.Errorf("the 409 carried Connection: %q; a client re-presenting its OWN spent reservation is confused, not hostile", got)
	}
	p.assertStaysOpen(alpha.token, "an agent re-presented its OWN spent reservation under a fresh key, which is a confused-but-honest client")
}

// TestCrossMintIsIndistinguishableFromAnHonestSpentReservation is CASE 2, and it
// deliberately asserts a LIMITATION rather than a policy.
//
// An operator asked for the 409 "no matching sequence reservation" to disconnect
// when the caller presents ANOTHER AGENT'S reservation. This test is the
// evidence that internal/httpapi cannot implement that, because the hostile
// request and the honest one produce the SAME sentinel, the SAME status and the
// SAME body:
//
//   - mallory, authenticated as itself, presents ALPHA's reservation under a key
//     mallory never minted  -> hub.ErrUnknownMint
//   - alpha, authenticated as itself, presents its OWN spent reservation under a
//     key it never minted    -> hub.ErrUnknownMint
//
// The lookup that produces both is h.mints[{agent, op, key}] in
// internal/hub/hub.go: it MISSES, and a miss carries no information about who —
// if anyone — the presented message id was minted for. The minting agent lives
// only inside the map KEY and never in the value, so the hub holds no index
// from message id to minting agent.
//
// Precise about what is and is not reachable, because the difference is the
// whole of the follow-up task: for a SPENT and still-retained message the owner
// is in principle recoverable from the store, but store.Since deliberately
// hides a message from its own sender (store.Message.VisibleTo returns false
// when agentID == m.Sender), it is retention-bounded, and answering ownership
// in a response would build a cross-agent sequence-space oracle. For an
// OUTSTANDING reservation — the case that actually matters, exercised by the
// second half of this test — the information exists ONLY in that unexported
// map, and nothing internal/httpapi can call reaches it.
//
// Disconnecting on the sentinel alone would therefore hit the honest client in
// TestSpentOwnReservationKeepsTheConnection above while believing it had aimed
// at the adversary. The fix belongs in internal/hub — a distinct sentinel raised
// only when the presented (message_id, seq) matches an OUTSTANDING reservation
// held by a different agent — and this test must be replaced by a disconnect
// assertion on the day that sentinel exists.
//
// It is written as an assertion, not a comment, so the claim is CHECKED: if the
// two responses ever stop being identical, the ambiguity has been resolved
// somewhere and this decision is due a re-read.
func TestCrossMintIsIndistinguishableFromAnHonestSpentReservation(t *testing.T) {
	srv, ts := socketBus(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")
	mallory := enrolAndAuthenticate(t, srv, "mallory")

	msgID, seq := mintOverHTTP(t, srv, alpha, "send", "cross-1")
	if rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend,
		signedSendBody(beta.id, b64("alpha's message"), "cross-1", alpha.id, msgID, seq)); rec.Code != http.StatusCreated {
		t.Fatalf("alpha's genuine send = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	// (a) THE ADVERSARY. mallory names ITSELF as sender — so the 403
	// sender-mismatch check cannot fire — and presents ALPHA's message id.
	pm := dialPinned(t, ts)
	pm.warmUp(mallory.token)
	theft := pm.do(http.MethodPost, httpapi.RouteSend, mallory.token,
		signedSendBody(beta.id, b64("mallory's payload"), "cross-mallory", mallory.id, msgID, seq))
	if theft.status != http.StatusConflict {
		t.Fatalf("mallory presenting alpha's reservation = %d, want 409; body %s", theft.status, theft.body)
	}

	// (b) THE HONEST CLIENT, in exactly the same shape.
	pa := dialPinned(t, ts)
	pa.warmUp(alpha.token)
	honest := pa.do(http.MethodPost, httpapi.RouteSend, alpha.token,
		signedSendBody(beta.id, b64("alpha again"), "cross-alpha-fresh", alpha.id, msgID, seq))
	if honest.status != http.StatusConflict {
		t.Fatalf("alpha re-presenting its own spent reservation = %d, want 409; body %s", honest.status, honest.body)
	}

	if got, want := errorOf(t, theft.body), errorOf(t, honest.body); got != want {
		t.Fatalf("the theft answered %q and the honest client answered %q; the two are no longer identical, so the ownership question may now be answerable and the disconnect decision is due a re-read", got, want)
	}

	// Neither is disconnected TODAY, and that is the conservative half of the
	// decision: an ambiguous case is never disconnected.
	pm.assertStaysOpen(mallory.token, "the cross-mint 409 is indistinguishable from an honest spent reservation, so it does not disconnect")
	pa.assertStaysOpen(alpha.token, "an honest client's spent reservation must never be disconnected")

	// (c) THE VARIANT THAT THE FOLLOW-UP TASK CAN ACTUALLY FIX: a reservation
	// that is still OUTSTANDING when a stranger presents it.
	//
	// It is separated from (a) because the two are not equally tractable. In (a)
	// alpha's mint was already spent and deleted, so the ownership fact is gone
	// from the hub entirely. Here it is still sitting in h.mints under alpha's
	// key, so a hub-side index COULD name the owner — which is exactly what the
	// follow-up proposes. Today it is still an undifferentiated 409, and this
	// assertion is what will go red when that lands.
	outstandingID, outstandingSeq := mintOverHTTP(t, srv, alpha, "send", "cross-outstanding")
	pv := dialPinned(t, ts)
	pv.warmUp(mallory.token)
	stolen := pv.do(http.MethodPost, httpapi.RouteSend, mallory.token,
		signedSendBody(beta.id, b64("stealing a live reservation"), "cross-mallory-2", mallory.id, outstandingID, outstandingSeq))
	if stolen.status != http.StatusConflict {
		t.Fatalf("mallory presenting alpha's OUTSTANDING reservation = %d, want 409; body %s", stolen.status, stolen.body)
	}
	if got, want := errorOf(t, stolen.body), errorOf(t, honest.body); got != want {
		t.Fatalf("spending a stranger's OUTSTANDING reservation answered %q but the honest client answered %q; the hub can now tell them apart, so internal/httpapi should disconnect the former", got, want)
	}
	pv.assertStaysOpen(mallory.token, "an ambiguous 409 is never disconnected; resolving this needs a distinct hub sentinel, not a guess here")
}
