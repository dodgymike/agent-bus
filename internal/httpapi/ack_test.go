package httpapi_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// newAckServer is newMessagingServer plus a REAL delivery lifecycle table.
//
// It is a separate builder rather than a flag on that one because every test in
// messages_test.go must keep running against a hub with NO lifecycle table —
// that is the production default for a build without one, and it is the
// equivalence hub.AckRecorder's doc promises. Nothing here is a double: a real
// WAL, a real auth service, a real hub and a real ack.Store, for the reason
// newMessagingServer gives at length.
func newAckServer(t *testing.T) (*httpapi.Server, *ack.Store, *bytes.Buffer) {
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
	roster := auth.NewMemoryRoster()
	svc, err := auth.NewService(auth.Options{Minter: minter, Roster: roster})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}

	acks := ack.NewStore(ack.Options{Logger: logger})
	if err := acks.Attach(walLog); err != nil {
		t.Fatalf("attaching the lifecycle table: %v", err)
	}

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
		Acks:      acks,
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
	return srv, acks, lg
}

// ackSignatureB64 is a well-formed 64-byte detached signature, standard base64.
//
// NO BUS VERIFIES IT AND NO BUS MAY CLAIM TO (§6.3): nothing distributes agents'
// messaging public keys, so a layer-3 attestation is end-to-end unverifiable by
// anybody today, including the sender (§16 Q1). What is enforced is SHAPE —
// present, exactly signing.SignatureSize bytes — byte-for-byte the posture
// already taken for message signatures. A real Ed25519 signature would therefore
// prove nothing here that a constant does not.
func ackSignatureB64() string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xCD}, signing.SignatureSize))
}

// ackFrame spells the request body a recipient sends.
func ackFrame(correlationKey, recipient, outcome, class string, withSignature bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `{"protocol_version":1,"correlation_key":%q,"recipient":%q,"outcome":%q,"emitted_at":1755000000000`,
		correlationKey, recipient, outcome)
	if class != "" {
		fmt.Fprintf(&sb, `,"class":%q`, class)
	}
	if withSignature {
		fmt.Fprintf(&sb, `,"attestation":{"signature":%q}`, ackSignatureB64())
	}
	sb.WriteString("}")
	return sb.String()
}

// deliveredFrame is the frame a well-behaved recipient sends.
func deliveredFrame(correlationKey, recipient string) string {
	return ackFrame(correlationKey, recipient, "delivered", "", true)
}

// sendForAck sends one DM and returns the correlation key, which on the ORIGIN
// bus IS the message's own id.
func sendForAck(t *testing.T, srv *httpapi.Server, from testAgent, to, key string) string {
	t.Helper()
	body := sendOK(t, srv, from, to, b64("a message to acknowledge"), key)
	id, _ := body["message_id"].(string)
	if id == "" {
		t.Fatalf("the send returned no message_id: %v", body)
	}
	return id
}

// TestAckRouteIsTheRecipientBoundary is the HTTP half of ACK-6: the wire shape,
// the status codes, and the authorization boundary a recipient actually meets.
//
// The hub-level state machine is proven by
// hub.TestRecipientAcknowledgementBoundary; this file proves the things only the
// route can be wrong about.
func TestAckRouteIsTheRecipientBoundary(t *testing.T) {
	t.Run("a recipient acknowledges its own message", func(t *testing.T) {
		srv, acks, _ := newAckServer(t)
		alpha := enrolAndAuthenticate(t, srv, "alpha")
		beta := enrolAndAuthenticate(t, srv, "beta")
		key := sendForAck(t, srv, alpha, beta.id, "k-ack-ok")

		rec := authed(t, srv, beta, http.MethodPost, httpapi.RouteAck, deliveredFrame(key, beta.id))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/ack = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
		body := decodeBody(t, rec)
		if body["accepted"] != true {
			t.Fatalf("accepted = %v, want true; body %s", body["accepted"], rec.Body.String())
		}
		if body["duplicate"] != false {
			t.Fatalf("duplicate = %v, want false", body["duplicate"])
		}
		if body["state"] != "delivered" {
			t.Fatalf("state = %v, want delivered", body["state"])
		}
		if _, present := body["class"]; present {
			t.Fatalf("a POSITIVE terminal carried a class: %s", rec.Body.String())
		}
		if r, ok := acks.Lookup(key, beta.id); !ok || r.State != ack.StateDelivered {
			t.Fatalf("the durable row is (%+v, %v), want delivered", r, ok)
		}

		// THE RETRY. Same key, same outcome: the original result, nothing
		// re-applied, 200 and NOT a 409, and nobody disconnected.
		again := authed(t, srv, beta, http.MethodPost, httpapi.RouteAck, deliveredFrame(key, beta.id))
		if again.Code != http.StatusOK {
			t.Fatalf("the retry = %d, want 200 (invariant 10's legitimate retry); body %s", again.Code, again.Body.String())
		}
		if b := decodeBody(t, again); b["duplicate"] != true || b["state"] != "delivered" {
			t.Fatalf("the retry answered %s, want duplicate=true state=delivered", again.Body.String())
		}
		if again.Header().Get("Connection") == "close" {
			t.Fatal("the retry closed the connection; §12 forbids any new disconnect on the ACK plane")
		}
	})

	t.Run("a refusal carries a class from the closed set", func(t *testing.T) {
		srv, acks, _ := newAckServer(t)
		alpha := enrolAndAuthenticate(t, srv, "alpha")
		beta := enrolAndAuthenticate(t, srv, "beta")
		key := sendForAck(t, srv, alpha, beta.id, "k-ack-nack")

		rec := authed(t, srv, beta, http.MethodPost, httpapi.RouteAck,
			ackFrame(key, beta.id, "refused", "recipient_refused_undecodable", true))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/ack = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
		body := decodeBody(t, rec)
		if body["state"] != "refused" || body["class"] != "recipient_refused_undecodable" {
			t.Fatalf("answered %s, want refused / recipient_refused_undecodable", rec.Body.String())
		}
		r, ok := acks.Lookup(key, beta.id)
		if !ok || r.State != ack.StateRefused || r.Class != ack.ClassRecipientRefusedUndecodable {
			t.Fatalf("the durable row is (%+v, %v), want refused with the recipient class", r, ok)
		}
		if r.AttestedBy != ack.AttestedByRecipientSignatureUnverified {
			t.Fatalf("the row is attested %q, want recipient_signature_unverified -- there is deliberately no value meaning `verified`", r.AttestedBy)
		}
	})

	t.Run("a conflicting terminal is 409 and keeps the connection", func(t *testing.T) {
		srv, acks, _ := newAckServer(t)
		alpha := enrolAndAuthenticate(t, srv, "alpha")
		beta := enrolAndAuthenticate(t, srv, "beta")
		key := sendForAck(t, srv, alpha, beta.id, "k-ack-conflict")

		if rec := authed(t, srv, beta, http.MethodPost, httpapi.RouteAck, deliveredFrame(key, beta.id)); rec.Code != http.StatusOK {
			t.Fatalf("the first ack = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
		rec := authed(t, srv, beta, http.MethodPost, httpapi.RouteAck,
			ackFrame(key, beta.id, "refused", "recipient_refused_policy", true))
		if rec.Code != http.StatusConflict {
			t.Fatalf("a second, DIFFERENT terminal = %d, want 409; body %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Connection") == "close" {
			t.Fatal("the conflict closed the connection; §12 rules that invariant 10's protocol-violation case is reject-and-log with NO disconnect")
		}
		if rec.Header().Get("Retry-After") != "" {
			t.Fatal("the conflict carried a Retry-After; re-sending can never succeed and dressing a permanent refusal as transient puts a client in an endless retry loop")
		}
		if r, _ := acks.Lookup(key, beta.id); r.State != ack.StateDelivered {
			t.Fatalf("the row is %s; the FIRST terminal must stand", r.State)
		}
	})

	t.Run("the uniform answer is the same for four different facts", func(t *testing.T) {
		srv, acks, _ := newAckServer(t)
		alpha := enrolAndAuthenticate(t, srv, "alpha")
		beta := enrolAndAuthenticate(t, srv, "beta")
		gamma := enrolAndAuthenticate(t, srv, "gamma")
		key := sendForAck(t, srv, alpha, beta.id, "k-ack-uniform")

		// gamma is enrolled, authenticated, and was never addressed. It acks as
		// ITSELF -- the only thing it can do, since the recipient is the
		// principal -- and must learn nothing about whether the key exists.
		notYours := authed(t, srv, gamma, http.MethodPost, httpapi.RouteAck, deliveredFrame(key, gamma.id))
		// A key that never existed at all.
		neverWas := authed(t, srv, gamma, http.MethodPost, httpapi.RouteAck,
			deliveredFrame(msgTestBusID+"-999999", gamma.id))
		// The SENDER acknowledging its own message: it was not addressed either.
		notAddressee := authed(t, srv, alpha, http.MethodPost, httpapi.RouteAck, deliveredFrame(key, alpha.id))
		// THE FOURTH FACT, and the one this subtest used to be missing: a
		// MALFORMED key. It is the HIGH finding of ACK-6's security gate --
		// relay.ValidatePeerAckRequest deliberately validates no ids (the PEER
		// route does that inside AuthorizePeerAck, which this route never calls),
		// so before the fix a malformed or ABSENT key reached ack.Store's own
		// validatePair and came back 500 with an unthrottled ERROR line, while an
		// unknown key came back 200 `unknown`. The subtest was named "four
		// different facts" and asserted three, which is exactly why it survived.
		malformed := authed(t, srv, gamma, http.MethodPost, httpapi.RouteAck,
			deliveredFrame(strings.Repeat("x", 3000), gamma.id))
		absent := authed(t, srv, gamma, http.MethodPost, httpapi.RouteAck,
			deliveredFrame("", gamma.id))

		for _, tc := range []struct {
			name string
			code int
			body string
		}{
			{"a key that is not yours", notYours.Code, notYours.Body.String()},
			{"a key that never existed", neverWas.Code, neverWas.Body.String()},
			{"the sender is not the addressee", notAddressee.Code, notAddressee.Body.String()},
			{"a malformed key", malformed.Code, malformed.Body.String()},
			{"an absent key", absent.Code, absent.Body.String()},
		} {
			if tc.code != http.StatusOK {
				t.Fatalf("%s = %d, want 200 with the uniform `unknown` (§13.3); body %s", tc.name, tc.code, tc.body)
			}
		}
		for _, other := range []struct {
			name string
			body string
		}{
			{"never was", neverWas.Body.String()},
			{"not addressee", notAddressee.Body.String()},
			{"malformed", malformed.Body.String()},
			{"absent", absent.Body.String()},
		} {
			if other.body != notYours.Body.String() {
				t.Fatalf("the %q refusal is DISTINGUISHABLE from the not-yours refusal, which is a message-existence oracle:\n  not yours: %s  %s: %s",
					other.name, notYours.Body.String(), other.name, other.body)
			}
		}
		b := decodeBody(t, notYours)
		if b["accepted"] != false || b["state"] != httpapi.AckStateUnknown {
			t.Fatalf("the uniform answer is %s, want accepted=false state=unknown", notYours.Body.String())
		}
		// AND NOTHING WAS CREATED. A boundary that minted a row here would let any
		// authenticated agent fill the table with keys it invented.
		if _, ok := acks.Lookup(key, gamma.id); ok {
			t.Fatal("a row was created for an agent that was never addressed")
		}
		if r, _ := acks.Lookup(key, beta.id); r.State != ack.StateAccepted {
			t.Fatalf("the real recipient's row is %s after three foreign acknowledgements, want accepted", r.State)
		}
	})

	t.Run("the recipient field must be the authenticated principal", func(t *testing.T) {
		srv, acks, _ := newAckServer(t)
		alpha := enrolAndAuthenticate(t, srv, "alpha")
		beta := enrolAndAuthenticate(t, srv, "beta")
		gamma := enrolAndAuthenticate(t, srv, "gamma")
		key := sendForAck(t, srv, alpha, beta.id, "k-ack-impersonate")

		// gamma names BETA in the frame. The field is a CLAIM; the principal is
		// the identity. This is the forgery ack.Store.Settle's doc names --
		// "without that, agent B can mark agent A's message refused".
		rec := authed(t, srv, gamma, http.MethodPost, httpapi.RouteAck, deliveredFrame(key, beta.id))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("acking as somebody else = %d, want 403; body %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Connection") == "close" {
			t.Fatal("the refusal closed the connection; there are no signed bytes to replay here and §12 forbids a new disconnect")
		}
		if r, _ := acks.Lookup(key, beta.id); r.State != ack.StateAccepted {
			t.Fatalf("beta's row is %s after gamma's attempt, want accepted", r.State)
		}
	})

	t.Run("malformed frames are 400 and write nothing", func(t *testing.T) {
		srv, acks, _ := newAckServer(t)
		alpha := enrolAndAuthenticate(t, srv, "alpha")
		beta := enrolAndAuthenticate(t, srv, "beta")
		key := sendForAck(t, srv, alpha, beta.id, "k-ack-malformed")

		for _, tc := range []struct {
			name string
			body string
		}{
			{"a BUS-EMITTED class on a recipient refusal", ackFrame(key, beta.id, "refused", "horizon_expired", true)},
			{"a refusal with no class", ackFrame(key, beta.id, "refused", "", true)},
			{"an unrecognised class", ackFrame(key, beta.id, "refused", "recipient_refused_because_i_said_so", true)},
			{"a class on a positive terminal", ackFrame(key, beta.id, "delivered", "recipient_refused_policy", true)},
			{"undeliverable is a routing claim an agent may not make", ackFrame(key, beta.id, "undeliverable", "no_route", false)},
			{"an unrecognised outcome", ackFrame(key, beta.id, "polled", "", true)},
			{"no attestation at all", ackFrame(key, beta.id, "delivered", "", false)},
			{"an empty attestation object", `{"protocol_version":1,"correlation_key":"` + key + `","recipient":"` + beta.id + `","outcome":"delivered","emitted_at":1755000000000,"attestation":{"signature":""}}`},
			{"a short signature", `{"protocol_version":1,"correlation_key":"` + key + `","recipient":"` + beta.id + `","outcome":"delivered","emitted_at":1755000000000,"attestation":{"signature":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 63)) + `"}}`},
			{"no emitted_at", `{"protocol_version":1,"correlation_key":"` + key + `","recipient":"` + beta.id + `","outcome":"delivered","attestation":{"signature":"` + ackSignatureB64() + `"}}`},
			{"an unsupported wire version", `{"protocol_version":9,"correlation_key":"` + key + `","recipient":"` + beta.id + `","outcome":"delivered","emitted_at":1755000000000,"attestation":{"signature":"` + ackSignatureB64() + `"}}`},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec := authed(t, srv, beta, http.MethodPost, httpapi.RouteAck, tc.body)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("= %d, want 400; body %s", rec.Code, rec.Body.String())
				}
				if rec.Header().Get("Connection") == "close" {
					t.Fatal("a malformed frame closed the connection; §12 forbids it")
				}
				if r, _ := acks.Lookup(key, beta.id); r.State != ack.StateAccepted {
					t.Fatalf("the row moved to %s; a rejected frame must write nothing", r.State)
				}
			})
		}
	})

	// THE SURFACE LABEL IS PROVEN BY WHICH LAYER REFUSES, NOT BY THE STATUS CODE.
	//
	// This subtest exists because a mutation found the gap: swapping
	// relay.AckSurfaceAgent for relay.AckSurfacePeer at the one call site left
	// every other assertion in this file GREEN. Under the peer label an
	// `undeliverable` frame validates — a peer is entitled to assert a routing
	// outcome — and is then refused one layer later by the hub, which rejects the
	// `peer_bus` attestation an agent cannot produce. Same 400, same body: the
	// defence in depth held, and the guard that was supposed to catch it could
	// not fire.
	//
	// So the assertion is on WHICH refusal ran. The frame-level line names the
	// frame; the hub-level line names the boundary. Asserting on our own debug
	// log is the weakest kind of assertion and it is used here deliberately,
	// because the alternative is no assertion at all: the surface constant is
	// currently belt-and-braces, and the day the hub's independent check is
	// relaxed it becomes the only thing standing between an agent and a durable
	// record saying an adjacent BUS vouched for a routing failure.
	t.Run("an agent may not assert a routing outcome, and the FRAME refuses it", func(t *testing.T) {
		srv, acks, logBuf := newAckServer(t)
		alpha := enrolAndAuthenticate(t, srv, "alpha")
		beta := enrolAndAuthenticate(t, srv, "beta")
		key := sendForAck(t, srv, alpha, beta.id, "k-ack-surface")

		rec := authed(t, srv, beta, http.MethodPost, httpapi.RouteAck,
			ackFrame(key, beta.id, "undeliverable", "no_route", false))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("an `undeliverable` from an agent = %d, want 400; body %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(logBuf.String(), "the frame is not one this bus will record") {
			t.Fatalf("the refusal did not come from the FRAME validation, which is where relay.AckSurfaceAgent is enforced; the surface label at the mount site may have been widened to the peer one\n--- log ---\n%s", logBuf.String())
		}
		if strings.Contains(logBuf.String(), "rejected by the hub boundary") {
			t.Fatalf("the frame reached the hub before being refused; on the agent surface `undeliverable` must not validate at all\n--- log ---\n%s", logBuf.String())
		}
		if r, _ := acks.Lookup(key, beta.id); r.State != ack.StateAccepted {
			t.Fatalf("the row moved to %s", r.State)
		}
	})

	t.Run("the route is authenticated and POST-only", func(t *testing.T) {
		srv, _, _ := newAckServer(t)
		beta := enrolAndAuthenticate(t, srv, "beta")

		anon := doRequest(t, srv, http.MethodPost, httpapi.RouteAck, deliveredFrame(msgTestBusID+"-1", beta.id), "application/json")
		if anon.Code != http.StatusUnauthorized {
			t.Fatalf("an anonymous POST = %d, want 401; every route authenticates except the four on the allow-list (invariant 3)", anon.Code)
		}
		get := authed(t, srv, beta, http.MethodGet, httpapi.RouteAck, "")
		if get.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s = %d, want 405", httpapi.RouteAck, get.Code)
		}
	})

	t.Run("the route is registered", func(t *testing.T) {
		srv, _, _ := newAckServer(t)
		found := false
		for _, p := range srv.Routes() {
			if p == httpapi.RouteAck {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s is not among the registered routes %v; a route registered outside s.route is invisible to the enumeration test", httpapi.RouteAck, srv.Routes())
		}
	})
}
