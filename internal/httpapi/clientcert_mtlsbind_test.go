package httpapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// Guards for the MTLS-BIND middleware clause: the client certificate on the
// connection is plumbed from r.TLS into the request context, so a handler can
// bind it (enrolment) or cross-check it (MTLS-CROSSCHECK) without reaching for
// the transport itself.
//
// # EACH GUARD IS MUTATION-TESTED ALONE
//
// Every test names the single mutation that turns it red, and each of those
// mutations was applied on its own and observed to fail EXACTLY the named test.
// This is a stated requirement on this task rather than a nicety: RELAY-20's
// equivalent property had two independent mechanisms behind it, so no single
// mutation went red, and an auditor flagged that as a latent trap — a guard you
// cannot make fail is a guard you cannot trust.

const ccProbePath = "/v1/info"

// ccServer builds a Server with a fixed clock and a debug log buffer. The clock
// is fixed because the validity check is judged against s.now(), and the test
// that proves expiry is enforced moves it.
func ccServer(t *testing.T, now time.Time) (*Server, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	srv := New(Options{
		Identity: StaticIdentity("bus-ccbind-test"),
		Logger:   logging.New(&logBuf, logging.LevelDebug),
		Now:      func() time.Time { return now },
	})
	return srv, &logBuf
}

// ccCert mints a REAL self-signed certificate, so every fingerprint here is
// computed from DER that a handshake would actually carry, never from a
// hand-written digest that could agree with a wrong implementation.
func ccCert(t *testing.T, id string) *x509.Certificate {
	t.Helper()
	m, err := buscert.LoadOrCreate(t.TempDir(), buscert.Options{BusID: id})
	if err != nil {
		t.Fatalf("buscert.LoadOrCreate(%s): %v", id, err)
	}
	return m.Certificate()
}

// ccValidNow returns an instant comfortably INSIDE cert's validity window.
//
// It is derived from the certificate rather than written as a literal date, and
// that is not fussiness: buscert mints with the real wall clock, so any
// hard-coded "now" drifts out of the window as the calendar moves and the test
// starts failing — or, far worse, starts passing for the wrong reason once the
// literal falls outside the window and the certificate is rejected by the very
// check a positive test is not looking at.
func ccValidNow(cert *x509.Certificate) time.Time {
	return cert.NotBefore.Add(time.Hour)
}

// ccRequest builds a request presenting certs as the client chain, or carrying
// no TLS state at all when certs is nil.
func ccRequest(certs []*x509.Certificate) *http.Request {
	req := httptest.NewRequest(http.MethodGet, ccProbePath, strings.NewReader(""))
	if certs != nil {
		req.TLS = &tls.ConnectionState{PeerCertificates: certs}
	}
	return req
}

// ccRun sends req through WithClientCertificate and reports what the inner
// handler saw, plus whether it ran at all.
func ccRun(t *testing.T, srv *Server, req *http.Request) (seen ClientCertificate, ok, ran bool, status int) {
	t.Helper()
	h := srv.WithClientCertificate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		seen, ok = ClientCertificateFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return seen, ok, ran, rec.Code
}

// TestClientCertificateReachesTheHandler is the positive path: a valid presented
// certificate arrives in the context, and its fingerprint is the ONE this
// codebase computes — buscert.FingerprintOf over the DER exactly as it arrived.
//
// MUTATION THAT KILLS IT ALONE: stop attaching the value (return before the
// context.WithValue), or compute the fingerprint any second way (over the SPKI,
// over a re-marshalled certificate, over a PEM encoding).
func TestClientCertificateReachesTheHandler(t *testing.T) {
	cert := ccCert(t, "agent-alpha")
	srv, _ := ccServer(t, ccValidNow(cert))

	seen, ok, ran, status := ccRun(t, srv, ccRequest([]*x509.Certificate{cert}))
	if !ran || status != http.StatusOK {
		t.Fatalf("handler ran=%v status=%d; this middleware admits every request", ran, status)
	}
	if !ok {
		t.Fatal("a valid client certificate was presented but no ClientCertificate reached the handler")
	}
	if seen.Fingerprint != buscert.FingerprintOf(cert) {
		t.Fatalf("fingerprint %s, want %s (sha256 over the DER exactly as it arrived)", seen.Fingerprint, buscert.FingerprintOf(cert))
	}
	if seen.Leaf != cert {
		t.Fatal("Leaf is not the certificate that was presented at index 0")
	}
}

// TestNoClientCertificateIsAdmittedWithNoIdentity pins the accepted-absence
// decision at the transport edge. The listener is tls.RequestClientCert, which
// REQUESTS and never REQUIRES, so a connection with no certificate is ordinary:
// /healthz, /v1/info, the container healthcheck and every client that has not
// grown a keypair yet.
//
// The request must be SERVED, and the context must carry NOTHING — the two
// halves matter separately. Serving-but-attaching-a-zero-value would give every
// certificate-less caller one shared "identity".
//
// MUTATION THAT KILLS IT ALONE: refuse the request when no certificate is
// presented (which would also take the bus's own health probe down); or attach a
// zero-valued ClientCertificate instead of nothing (caught by the ok check).
func TestNoClientCertificateIsAdmittedWithNoIdentity(t *testing.T) {
	srv, _ := ccServer(t, ccValidNow(ccCert(t, "agent-clockref")))

	for _, tc := range []struct {
		name  string
		certs []*x509.Certificate
	}{
		{"no TLS state at all", nil},
		{"TLS but an empty peer chain", []*x509.Certificate{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := ccRequest(nil)
			if tc.certs != nil {
				req.TLS = &tls.ConnectionState{PeerCertificates: tc.certs}
			}
			_, ok, ran, status := ccRun(t, srv, req)
			if !ran || status != http.StatusOK {
				t.Fatalf("handler ran=%v status=%d; absence of a client certificate is a NORMAL case and must be admitted", ran, status)
			}
			if ok {
				t.Fatal("a ClientCertificate was attached although none was presented")
			}
		})
	}
}

// TestExpiredClientCertificateIsIgnored is the RELAY-20 hole closed on the AGENT
// plane. crypto/tls proves possession of the leaf's private key and NEVER looks
// at NotBefore or NotAfter, so an expired certificate completes the handshake
// and arrives here exactly like a fresh one.
//
// Without this check, enrolment would mint a DURABLE binding for a credential
// that has already expired, and expiry — the only automatic bound on a leaked
// client key — would mean nothing on this plane.
//
// The certificate is real and valid; the SERVER'S CLOCK is moved past its
// NotAfter, which is the honest way to age a certificate without hand-building
// a malformed one.
//
// MUTATION THAT KILLS IT ALONE: delete the checkClientCertValidity call from
// WithClientCertificate. Nothing else on the agent plane checks dates, so this
// test is the only thing standing on that line — which is the point.
func TestExpiredClientCertificateIsIgnored(t *testing.T) {
	cert := ccCert(t, "agent-stale")
	// Comfortably past NotAfter, and asserted rather than assumed: a "future"
	// that fell inside the validity window would make this test vacuous.
	future := cert.NotAfter.Add(24 * time.Hour)
	if !future.After(cert.NotAfter) {
		t.Fatalf("test setup: %v is not after NotAfter %v", future, cert.NotAfter)
	}
	srv, logBuf := ccServer(t, future)

	seen, ok, ran, status := ccRun(t, srv, ccRequest([]*x509.Certificate{cert}))
	if !ran || status != http.StatusOK {
		t.Fatalf("handler ran=%v status=%d; this middleware ignores an unusable certificate, it does not refuse the request", ran, status)
	}
	if ok {
		t.Fatalf("an EXPIRED certificate produced a transport identity (%s); it must bind nothing and satisfy no cross-check", seen.Fingerprint)
	}
	// And it is LOGGED, specifically. A silently ignored certificate is
	// indistinguishable from one that was never sent, and the agent cannot see
	// the difference either — its request SUCCEEDS, unbound.
	if !strings.Contains(logBuf.String(), "outside its own validity window") {
		t.Fatalf("the discard was not logged; log was:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), buscert.FingerprintOf(cert).String()) {
		t.Fatal("the log line does not name the fingerprint, which is the value an operator needs to correlate it with a roster binding")
	}
}

// TestNotYetValidClientCertificateIsIgnored is the other half of the window, and
// it is a separate test because a check written as `now.After(NotAfter)` passes
// the expiry test and fails this one.
//
// MUTATION THAT KILLS IT ALONE: replace checkClientCertValidity's x509 verdict
// with a local `at.After(leaf.NotAfter)` comparison — the exact shortcut
// invariant 9 forbids, which looks correct and silently accepts every
// not-yet-valid certificate.
func TestNotYetValidClientCertificateIsIgnored(t *testing.T) {
	cert := ccCert(t, "agent-early")
	past := cert.NotBefore.Add(-24 * time.Hour)
	if !past.Before(cert.NotBefore) {
		t.Fatalf("test setup: %v is not before NotBefore %v", past, cert.NotBefore)
	}
	srv, _ := ccServer(t, past)

	_, ok, ran, status := ccRun(t, srv, ccRequest([]*x509.Certificate{cert}))
	if !ran || status != http.StatusOK {
		t.Fatalf("handler ran=%v status=%d", ran, status)
	}
	if ok {
		t.Fatal("a NOT-YET-VALID certificate produced a transport identity")
	}
}

// TestOnlyTheLeafIsConsidered is the chain-spoofing guard, and it is the one
// whose absence would be worth the most to an attacker.
//
// The client controls EVERY certificate it sends, while the handshake's
// CertificateVerify proves possession of the LEAF's private key ALONE. So an
// attacker presents its own certificate at index 0 — which it can prove it holds
// — and appends the VICTIM'S PUBLIC certificate at index 1, which is public data
// anyone can obtain. A middleware that searched the chain for a bound
// fingerprint would hand over the victim's agent id.
//
// MUTATION THAT KILLS IT ALONE: iterate r.TLS.PeerCertificates in
// presentedClientLeaf instead of returning index [0].
func TestOnlyTheLeafIsConsidered(t *testing.T) {
	attacker := ccCert(t, "agent-attacker")
	victim := ccCert(t, "agent-victim")
	// Judged at an instant valid for the LEAF, which is the only certificate
	// that may ever be considered.
	srv, _ := ccServer(t, ccValidNow(attacker))

	seen, ok, _, _ := ccRun(t, srv, ccRequest([]*x509.Certificate{attacker, victim}))
	if !ok {
		t.Fatal("the leaf at index 0 should still have been accepted")
	}
	if seen.Fingerprint != buscert.FingerprintOf(attacker) {
		t.Fatalf("fingerprint %s, want the LEAF's %s", seen.Fingerprint, buscert.FingerprintOf(attacker))
	}
	if seen.Fingerprint == buscert.FingerprintOf(victim) {
		t.Fatal("the appended certificate at index 1 was used; only index [0] may ever be considered")
	}
}

// TestClientCertificateAccessorRefusesAnEmptyContext: the accessor is fail-
// closed on a context no middleware ever touched. ok == false there means "no
// transport identity", never "no restriction applies".
//
// MUTATION THAT KILLS IT ALONE: drop the type assertion in
// ClientCertificateFromContext and return the zero value with ok == true.
func TestClientCertificateAccessorRefusesAnEmptyContext(t *testing.T) {
	if _, ok := ClientCertificateFromContext(nil); ok {
		t.Fatal("a nil context yielded a client certificate")
	}
	if _, ok := ClientCertificateFromContext(httptest.NewRequest(http.MethodGet, "/", nil).Context()); ok {
		t.Fatal("a context the middleware never ran on yielded a client certificate")
	}
	if _, ok := ClientCertFingerprintFromContext(nil); ok {
		t.Fatal("a nil context yielded a fingerprint")
	}
}

// TestClientCertCtxKeyDoesNotCollide pins the context key values against each
// other.
//
// The task text asserted the next free ctxKey was 2. IT WAS NOT — peerprincipal.
// go had already taken 2 — and this is the guard that makes the next such
// mistake fail loudly. It matters because these keys are compared BY VALUE:
// duplicating one does not fail to compile, it silently shadows the other's
// entry, so the wrong type comes back, the accessor's type assertion turns it
// into "absent", and a cross-check that should have run simply does not.
//
// MUTATION THAT KILLS IT ALONE: change ctxKeyClientCert to 0, 1 or 2.
func TestClientCertCtxKeyDoesNotCollide(t *testing.T) {
	keys := map[ctxKey]string{
		ctxKeyRequestID:     "ctxKeyRequestID (middleware.go)",
		ctxKeyPrincipal:     "ctxKeyPrincipal (authmw.go)",
		ctxKeyPeerPrincipal: "ctxKeyPeerPrincipal (peerprincipal.go)",
	}
	if owner, taken := keys[ctxKeyClientCert]; taken {
		t.Fatalf("ctxKeyClientCert = %d collides with %s; context keys are compared by value and a collision silently shadows, it does not fail to compile", ctxKeyClientCert, owner)
	}
}

// TestClientCertificateIsMountedInTheServerChain proves the middleware is
// actually WIRED, not merely written.
//
// This is a distinct failure from every test above: WithClientCertificate can be
// perfect and, if server.go does not mount it, no handler ever sees a
// certificate and enrolment binds nothing — with every unit test above still
// green. It goes through srv.Handler(), the real chain the listener serves.
//
// MUTATION THAT KILLS IT ALONE: revert server.go's chain to
// LoggingMiddleware(s.log, s.authMiddleware(mux)).
func TestClientCertificateIsMountedInTheServerChain(t *testing.T) {
	cert := ccCert(t, "agent-mounted")
	srv, _ := ccServer(t, ccValidNow(cert))

	// A route that reports what the mounted chain delivered. It is registered
	// through the same probe path the rest of this file uses; /v1/info is
	// unauthenticated, which is also the point — the middleware must sit OUTSIDE
	// authMiddleware, or enrolment (also unauthenticated) could never bind.
	req := ccRequest([]*x509.Certificate{cert})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s over a client certificate gave %d, want 200", ccProbePath, rec.Code)
	}

	// The observable proof that the chain ran the middleware: the same request
	// with an EXPIRED certificate logs the specific discard line, which only
	// WithClientCertificate emits and which nothing reaches unless it is mounted.
	stale := ccCert(t, "agent-mounted-stale")
	srvStale, logBuf := ccServer(t, stale.NotAfter.Add(24*time.Hour))
	recStale := httptest.NewRecorder()
	srvStale.ServeHTTP(recStale, ccRequest([]*x509.Certificate{stale}))
	if !strings.Contains(logBuf.String(), "outside its own validity window") {
		t.Fatalf("WithClientCertificate is not mounted in the server handler chain; log was:\n%s", logBuf.String())
	}
}

// ccEnrolServer builds a Server with a REAL auth service over a MemoryRoster the
// test also holds, so the end-to-end test can read the STORED binding rather
// than infer it. The real service is used rather than a stub because the thing
// under test is precisely that the handler and the service agree about where the
// fingerprint comes from.
func ccEnrolServer(t *testing.T, now time.Time) (*Server, *auth.MemoryRoster) {
	t.Helper()
	minter, err := ids.NewAgentIDMinter("bus-ccenrol-test", ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	roster := auth.NewMemoryRoster()
	svc, err := auth.NewService(auth.Options{Minter: minter, Roster: roster})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}
	var logBuf bytes.Buffer
	srv := New(Options{
		Identity: StaticIdentity("bus-ccenrol-test"),
		Logger:   logging.New(&logBuf, logging.LevelDebug),
		Auth:     svc,
		Now:      func() time.Time { return now },
	})
	return srv, roster
}

// ccEnrol posts a valid enrolment over a connection presenting certs (nil for
// none) and returns the response.
func ccEnrol(t *testing.T, srv *Server, name, idemKey string, certs []*x509.Certificate) *httptest.ResponseRecorder {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	body := fmt.Sprintf(`{"name":%q,"public_key":%q,"idempotency_key":%q}`,
		name, base64.StdEncoding.EncodeToString(pub), idemKey)
	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if certs != nil {
		req.TLS = &tls.ConnectionState{PeerCertificates: certs}
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestEnrolOverHTTPBindsTheConnectionCertificate is the task end to end, through
// the REAL handler chain: POST /v1/enroll over a connection presenting a client
// certificate records that certificate's fingerprint against the agent id the
// server minted, and the binding is resolvable the way a cross-check will
// resolve it.
//
// It is deliberately separate from the service-level test in internal/auth. That
// one proves auth.Enrol binds what it is GIVEN; this one proves the HTTP layer
// gives it the right thing — the fingerprint from r.TLS, not from the body —
// and that the middleware, the handler and the service are actually connected.
// Either could be perfect while the wiring between them is missing.
//
// MUTATION THAT KILLS IT ALONE: drop ClientCertFingerprint from the
// auth.EnrolRequest literal in handleEnrol, or make enrolCertFingerprint return
// nil.
func TestEnrolOverHTTPBindsTheConnectionCertificate(t *testing.T) {
	cert := ccCert(t, "agent-enrolling")
	srv, roster := ccEnrolServer(t, ccValidNow(cert))

	rec := ccEnrol(t, srv, "alpha", "k-1", []*x509.Certificate{cert})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/enroll gave %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the 201 body %q: %v", rec.Body.String(), err)
	}
	if body.AgentID == "" {
		t.Fatalf("the 201 body carries no agent_id: %s", rec.Body.String())
	}

	// THE BINDING IS STORED, against the id the SERVER minted (invariant 1).
	entry, ok := roster.Get(body.AgentID)
	if !ok {
		t.Fatalf("agent %q is not in the roster", body.AgentID)
	}
	if len(entry.CertBindings) != 1 {
		t.Fatalf("CertBindings = %+v, want exactly one live binding", entry.CertBindings)
	}
	if entry.CertBindings[0].Fingerprint != [32]byte(buscert.FingerprintOf(cert)) {
		t.Fatalf("bound fingerprint %x, want the presented certificate's %s",
			entry.CertBindings[0].Fingerprint[:4], buscert.FingerprintOf(cert))
	}

	// AND IT RESOLVES BACK, which is the whole point of storing it.
	got, err := srv.Auth().AgentIDForClientCertificate([32]byte(buscert.FingerprintOf(cert)))
	if err != nil || got != body.AgentID {
		t.Fatalf("AgentIDForClientCertificate = (%q, %v), want (%q, nil)", got, err, body.AgentID)
	}
}

// TestEnrolOverHTTPWithoutACertificateSucceedsUnbound is the accepted-absence
// decision at the route an agent actually uses. It must keep working: no client
// ships a keypair yet (MTLS-CLIENTCERT is open), and a 4xx here would lock every
// one of them off the bus with no migration path.
//
// MUTATION THAT KILLS IT ALONE: make handleEnrol refuse when
// enrolCertFingerprint returns nil.
func TestEnrolOverHTTPWithoutACertificateSucceedsUnbound(t *testing.T) {
	srv, roster := ccEnrolServer(t, ccValidNow(ccCert(t, "agent-clockref")))

	rec := ccEnrol(t, srv, "alpha", "k-1", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/enroll with no client certificate gave %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the 201 body: %v", err)
	}
	entry, ok := roster.Get(body.AgentID)
	if !ok {
		t.Fatalf("agent %q is not in the roster", body.AgentID)
	}
	if len(entry.CertBindings) != 0 {
		t.Fatalf("nothing was presented, so nothing may be bound; got %+v", entry.CertBindings)
	}
}

// TestEnrolOverHTTPRefusesACertificateBoundToAnotherAgent: the uniqueness rule
// as a client experiences it — 409, and the reply does NOT name the agent that
// already holds the binding.
//
// That last assertion is the security-relevant half. Naming the holder would
// turn /v1/enroll — an UNAUTHENTICATED route — into an oracle mapping any
// certificate a caller possesses to an agent id on this bus.
//
// MUTATION THAT KILLS IT ALONE: return the auth error's text (which does name
// the agent) as the response body instead of the fixed string.
func TestEnrolOverHTTPRefusesACertificateBoundToAnotherAgent(t *testing.T) {
	cert := ccCert(t, "agent-shared")
	srv, _ := ccEnrolServer(t, ccValidNow(cert))

	first := ccEnrol(t, srv, "alpha", "k-1", []*x509.Certificate{cert})
	if first.Code != http.StatusCreated {
		t.Fatalf("first enrolment gave %d, want 201: %s", first.Code, first.Body.String())
	}
	var firstBody struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decoding the first 201 body: %v", err)
	}

	second := ccEnrol(t, srv, "beta", "k-2", []*x509.Certificate{cert})
	if second.Code != http.StatusConflict {
		t.Fatalf("second enrolment with the SAME client certificate gave %d, want 409: %s", second.Code, second.Body.String())
	}
	if strings.Contains(second.Body.String(), firstBody.AgentID) {
		t.Fatalf("the 409 body names the agent that already holds the binding (%q), turning an unauthenticated route into an id oracle: %s", firstBody.AgentID, second.Body.String())
	}
}

// TestEnrolOverHTTPIgnoresAnExpiredCertificate joins the two halves: the
// middleware's validity check and the enrolment binding. An expired certificate
// must not become a DURABLE binding that outlives the credential it names.
//
// It is a separate test from TestExpiredClientCertificateIsIgnored because that
// one proves the middleware attaches nothing, and this one proves the
// consequence that actually matters — nothing is written.
//
// MUTATION THAT KILLS IT ALONE: delete the checkClientCertValidity call from
// WithClientCertificate. (Two tests die on that one mutation, which is the safe
// direction: they assert different consequences of the same guard, rather than
// two guards defending one property.)
func TestEnrolOverHTTPIgnoresAnExpiredCertificate(t *testing.T) {
	cert := ccCert(t, "agent-expired-enrol")
	srv, roster := ccEnrolServer(t, cert.NotAfter.Add(24*time.Hour))

	rec := ccEnrol(t, srv, "alpha", "k-1", []*x509.Certificate{cert})
	if rec.Code != http.StatusCreated {
		t.Fatalf("enrolment over an expired certificate gave %d, want 201 (unbound, not refused): %s", rec.Code, rec.Body.String())
	}
	var body struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the 201 body: %v", err)
	}
	entry, ok := roster.Get(body.AgentID)
	if !ok {
		t.Fatalf("agent %q is not in the roster", body.AgentID)
	}
	if len(entry.CertBindings) != 0 {
		t.Fatalf("an EXPIRED certificate was bound durably: %+v", entry.CertBindings)
	}
	if got, err := srv.Auth().AgentIDForClientCertificate([32]byte(buscert.FingerprintOf(cert))); err == nil {
		t.Fatalf("the expired certificate resolves to %q", got)
	}
}
