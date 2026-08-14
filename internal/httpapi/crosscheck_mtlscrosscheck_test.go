package httpapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// Guards for MTLS-CROSSCHECK: invariant 11's cross-check, where mTLS and the
// session token finally meet. A credential is refused unless the CONNECTION it
// arrived over carries the client certificate that credential's agent is bound
// to.
//
// # EVERY GUARD HERE WAS WATCHED FAILING, AND THE ABSENCE ARM MOST OF ALL
//
// Reading a test tells you what it asserts, not whether it CAN fail. Each test
// below carries a "MUTATION THAT KILLS IT ALONE" line naming one edit to the
// shipped source; every one of those mutations was applied ON ITS OWN, run,
// observed to fail EXACTLY the named test, and reverted. The transcript is in the
// task's report note. This discipline is not decoration: three guards on the
// antecedent task could not fail, and every one was caught by mutating the source
// rather than by reading the test.
//
// THE ARM THAT MATTERS MOST IS ABSENCE, and it has a specific trap. The listener
// is tls.RequestClientCert, which REQUESTS a client certificate and never
// REQUIRES one, so a rule shaped
//
//	if a certificate is PRESENT it must match
//
// is defeated by presenting none — and every positive test passes either way.
// TestCrossCheckAbsentCertificateIsRefusedForABoundAgent is the test that
// separates that evadable form from the shipped one, and it was verified RED
// against exactly that rewrite of guard B's condition.
//
// # HOW A REQUEST'S FATE IS OBSERVED
//
// xcProbePath is registered NOWHERE and is NOT on the allow-list, which is what
// makes it a useful probe of an AUTHENTICATED route: the status alone says who
// decided.
//
//	403 -> this cross-check refused, before the mux was ever consulted.
//	404 -> the middleware admitted the request and handleNotFound (the only
//	       producer of a 404 here) ran, so the gate passed it through.
//
// On /v1/session/begin the evidence is stronger still: a refusal must leave the
// session table EMPTY, so "the handler did not run" is observable rather than
// inferred.
//
// Certificates are REAL throughout — minted by ccCert (clientcert_mtlsbind_test.
// go) via buscert.LoadOrCreate — so every fingerprint is computed from DER a
// handshake would actually carry, never from a hand-written digest that could
// agree with a wrong implementation. Instants are derived from the certificate
// with ccValidNow, never written as literal dates.

// xcBusID is the bus half of every agent id here. It satisfies ids.BusIDPattern,
// which the /v1/session/begin path parses.
const xcBusID = "bus-xcheck-test"

// xcProbePath is an AUTHENTICATED route: absent from unauthenticatedRoutes and
// registered on no mux in this package's tests. See the file header for how its
// status is read.
const xcProbePath = "/v1/messages/send"

// xcBus is a Server wired to a REAL auth.Service over a roster the test also
// holds, plus the log buffer every refusal line lands in.
//
// The service is real rather than a stub for the reason newAuthMWServer gives:
// the thing under test is precisely that the gate and internal/auth agree about
// what a binding is, and a stub would let them drift with nothing noticing.
type xcBus struct {
	t      *testing.T
	srv    *Server
	svc    *auth.Service
	logBuf *bytes.Buffer
}

// xcNewBus builds a bus whose clock reads now, over roster (nil for a fresh
// MemoryRoster).
func xcNewBus(t *testing.T, now time.Time, roster auth.Roster) *xcBus {
	t.Helper()
	minter, err := ids.NewAgentIDMinter(xcBusID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	if roster == nil {
		roster = auth.NewMemoryRoster()
	}
	svc, err := auth.NewService(auth.Options{
		Minter: minter,
		Roster: roster,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}
	var logBuf bytes.Buffer
	srv := New(Options{
		Identity: StaticIdentity(xcBusID),
		Logger:   logging.New(&logBuf, logging.LevelDebug),
		Auth:     svc,
		Now:      func() time.Time { return now },
	})
	return &xcBus{t: t, srv: srv, svc: svc, logBuf: &logBuf}
}

// enrol registers an agent through the REAL service, binding cert when one is
// given, and returns the server-minted id (invariant 1) with the private half of
// its auth keypair so a session can be established for it.
//
// It goes through auth.Service.Enrol rather than POST /v1/enroll deliberately:
// /v1/enroll is where a binding is CREATED and is not gated by this cross-check
// (requiring one there would be circular), so driving setup through HTTP would
// add a second moving part to every test without exercising anything this task
// changed. The HTTP binding path has its own guard, in
// clientcert_mtlsbind_test.go.
func (b *xcBus) enrol(name, idemKey string, cert *x509.Certificate) (agentID string, priv ed25519.PrivateKey) {
	b.t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	req := auth.EnrolRequest{Name: name, PublicKey: pub, IdempotencyKey: idemKey}
	if cert != nil {
		fp := [32]byte(buscert.FingerprintOf(cert))
		req.ClientCertFingerprint = &fp
	}
	res, err := b.svc.Enrol(req)
	if err != nil {
		b.t.Fatalf("Enrol(%s): %v", name, err)
	}
	return res.AgentID, priv
}

// activeToken performs the session handshake through the SERVICE, returning a
// live bearer token.
//
// It bypasses the two HTTP session routes on purpose. Those routes are THEMSELVES
// gated by the cross-check, so obtaining a token over HTTP would make every
// authenticated-route test depend on the very gate it is measuring — and a
// mutation that disabled the gate at /v1/session/begin would then silently repair
// the setup of the test meant to catch it. The gate ON those routes is proved
// separately, by TestCrossCheckGatesSessionBegin and
// TestCrossCheckGatesSessionComplete.
func (b *xcBus) activeToken(agentID string, priv ed25519.PrivateKey) string {
	b.t.Helper()
	ch, err := b.svc.BeginSession(agentID)
	if err != nil {
		b.t.Fatalf("BeginSession(%s): %v", agentID, err)
	}
	sig := ed25519.Sign(priv, []byte(auth.SessionSigningContext+ch.Token))
	if _, err := b.svc.CompleteSession(ch.Token, sig); err != nil {
		b.t.Fatalf("CompleteSession: %v", err)
	}
	return ch.Token
}

// do serves req through the REAL handler chain — LoggingMiddleware,
// WithClientCertificate, authMiddleware, the mux — which is the only way the
// certificate on the connection reaches the gate at all.
func (b *xcBus) do(req *http.Request) *httptest.ResponseRecorder {
	b.t.Helper()
	rec := httptest.NewRecorder()
	b.srv.ServeHTTP(rec, req)
	return rec
}

// xcCerts is the client chain to present: nil for a connection with no TLS state
// at all, an EMPTY (non-nil) slice for TLS with no peer certificate.
func xcCerts(certs ...*x509.Certificate) []*x509.Certificate { return certs }

// xcReq builds a request presenting certs (nil for no TLS at all).
func xcReq(method, path, body string, certs []*x509.Certificate) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if certs != nil {
		req.TLS = &tls.ConnectionState{PeerCertificates: certs}
	}
	return req
}

// xcBeginBody renders a /v1/session/begin body, through encoding/json so an
// agent id containing anything at all is escaped correctly rather than breaking
// the JSON — which matters for the oversized-id test.
func xcBeginBody(t *testing.T, agentID string) string {
	t.Helper()
	b, err := json.Marshal(SessionBeginRequestBody{AgentID: agentID})
	if err != nil {
		t.Fatalf("marshalling the session/begin body: %v", err)
	}
	return string(b)
}

// xcBegin posts /v1/session/begin for agentID over certs.
func (b *xcBus) begin(agentID string, certs []*x509.Certificate) *httptest.ResponseRecorder {
	b.t.Helper()
	return b.do(xcReq(http.MethodPost, RouteSessionBegin, xcBeginBody(b.t, agentID), certs))
}

// xcWantRefused asserts the standard refusal: 403, the ONE fixed body, no
// WWW-Authenticate challenge and no "Connection: close".
//
// The last two are not incidental. A 401-with-challenge would invite a refused
// caller to retry with a different bearer token, advertising the very credential
// confusion this gate prevents — and it would be a lie, since the wrong half of
// the pair is the connection's certificate, which no header can change. And
// invariant 10 forbids the disconnect outright: a merely BUGGY client (an agent
// that regenerated its keypair without re-enrolling) reaches this line on every
// request, and the socket carries other principals' traffic, including parked
// long polls.
func xcWantRefused(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%s: status = %d, want 403; body %s", what, rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: decoding the refusal body %q: %v", what, rec.Body.String(), err)
	}
	if body.Error != crossCheckRefusal {
		t.Fatalf("%s: refusal reason %q, want the one fixed string %q", what, body.Error, crossCheckRefusal)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("%s: the refusal carries WWW-Authenticate %q; a 403 here must not challenge for another bearer token", what, got)
	}
	if got := rec.Header().Get("Connection"); strings.EqualFold(got, "close") {
		t.Fatalf("%s: the refusal disconnects (Connection: %q); invariant 10 forbids it here", what, got)
	}
}

// xcRosterEntry builds a FULLY VALID roster entry, for the states no exported
// write path will create.
//
// Name is parsed back out of the id rather than written by hand because the
// durable roster refuses an entry whose Name disagrees with the name inside its
// AgentID (validateRosterEntry), and that failure lands during recovery, long
// before the rule under test.
func xcRosterEntry(t *testing.T, agentID string, bindings ...auth.CertBinding) auth.RosterEntry {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	_, name, _, err := ids.ParseAgentID(agentID)
	if err != nil {
		t.Fatalf("test fixture: agent id %q does not parse: %v", agentID, err)
	}
	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	return auth.RosterEntry{
		AgentID:       agentID,
		Name:          name,
		AuthPublicKey: pub,
		Epoch:         at,
		EnrolledAt:    at,
		CertBindings:  bindings,
	}
}

// xcLive and xcRetired build the two binding states over a REAL certificate's
// fingerprint.
func xcLive(cert *x509.Certificate) auth.CertBinding {
	return auth.CertBinding{
		Fingerprint: [32]byte(buscert.FingerprintOf(cert)),
		BoundAt:     time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
	}
}

func xcRetired(cert *x509.Certificate) auth.CertBinding {
	b := xcLive(cert)
	retired := b.BoundAt.Add(time.Hour)
	b.RetiredAt = &retired
	return b
}

// xcRecoveredRoster returns a WAL-backed roster rebuilt from a log carrying
// exactly the given entries.
//
// THIS IS THE ONLY WAY TO BUILD AN AMBIGUOUS BINDING, and it is how production
// reaches one. MemoryRoster.Put and WALRoster.put both REFUSE to make one
// certificate live on two agents (checkCertFingerprintUnbound), so the state
// cannot be created through any write path — but WALRoster.Apply does not run
// that check, because Apply is RECOVERY, replaying records that are already
// durable, and refusing one there would not un-write it, it would only turn a
// damaged log into an outage (invariant 6). So the records are written through a
// log with NO applier — nothing interprets or refuses them on the way past —
// and the roster is then rebuilt from that log alone.
func xcRecoveredRoster(t *testing.T, entries ...auth.RosterEntry) *auth.WALRoster {
	t.Helper()
	dir := t.TempDir()

	l, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	for _, e := range entries {
		body, err := auth.Encode(e)
		if err != nil {
			t.Fatalf("auth.Encode(%s): %v", e.AgentID, err)
		}
		if _, err := l.Write(wal.Entry{Kind: auth.RecordKind, Body: body}); err != nil {
			t.Fatalf("writing %s: %v", e.AgentID, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("closing the seeding log: %v", err)
	}

	r := auth.NewWALRoster(nil)
	l2, err := wal.Open(wal.LogOptions{Dir: dir, Applier: r})
	if err != nil {
		t.Fatalf("reopening the log for recovery: %v", err)
	}
	if err := r.Attach(l2); err != nil {
		l2.Close()
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { l2.Close() })
	for _, e := range entries {
		if _, ok := r.Get(e.AgentID); !ok {
			t.Fatalf("%s did not recover; the test proves nothing if the seeded state is not there", e.AgentID)
		}
	}
	return r
}

// ---------------------------------------------------------------------------
// THE ABSENCE ARM — the headline, and the one the whole task exists for.
// ---------------------------------------------------------------------------

// TestCrossCheckAbsentCertificateIsRefusedForABoundAgent: an agent that HAS a
// live binding, on a connection carrying NO client certificate, is REFUSED.
//
// This is the evasion the task closes. The listener REQUESTS a client
// certificate and never REQUIRES one, so "if a certificate is present it must
// match" is defeated by presenting none, and a stolen credential would replay
// from anywhere forever. Guard A cannot help: there is no fingerprint to
// resolve, so it never runs.
//
// Both spellings of absence are covered, because they arrive by different routes
// and a middleware that handled one could still panic or fall open on the other:
// no TLS state at all (a plaintext or in-process connection), and TLS with an
// EMPTY peer chain (the ordinary shape of a handshake where the client declined
// the certificate request).
//
// It also carries case 12: NOTHING IS MINTED. A refused begin must not leave a
// challenge behind — /v1/session/begin is unauthenticated, and an anonymous
// caller must not be able to create server state with a lifetime for an agent
// whose certificate it does not hold. The empty session table is the observable
// proof that the handler did not run at all.
//
// MUTATION THAT KILLS IT ALONE (either of two, each verified separately):
//   - delete guard B's whole `if` block from enforceCertBinding; or
//   - THE TRAP — rewrite guard B's condition to the evadable form
//     `if len(live) > 0 && present && !containsFingerprint(live, [32]byte(fp))`.
//     Verified RED under exactly that rewrite, which is what distinguishes this
//     test from one that merely looks like it covers absence.
func TestCrossCheckAbsentCertificateIsRefusedForABoundAgent(t *testing.T) {
	cert := ccCert(t, "agent-bound")

	for _, tc := range []struct {
		name  string
		certs []*x509.Certificate
	}{
		{"no TLS state at all", nil},
		{"TLS but an empty peer chain", xcCerts()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := xcNewBus(t, ccValidNow(cert), nil)
			agentID, _ := bus.enrol("alpha", "k-1", cert)

			rec := bus.begin(agentID, tc.certs)
			xcWantRefused(t, rec, "session/begin for a bound agent over no certificate")

			// The handler did not run: no token was returned...
			if strings.Contains(rec.Body.String(), "token") {
				t.Fatalf("the refusal body mentions a token: %s", rec.Body.String())
			}
			// ...and, decisively, nothing was minted (case 12).
			if n := bus.svc.SessionCount(); n != 0 {
				t.Fatalf("a REFUSED session/begin left %d session(s) in the table; a refusal must mint nothing at all", n)
			}
			// The refusal is LOGGED, specifically and loudly, naming which guard
			// fired and that no certificate was presented. A silent refusal is
			// indistinguishable from a bug to the operator who has to explain it.
			if log := bus.logBuf.String(); !strings.Contains(log, "required-certificate-not-presented") ||
				!strings.Contains(log, "client_cert_presented=false") {
				t.Fatalf("the refusal was not logged specifically; log was:\n%s", log)
			}
		})
	}
}

// TestCrossCheckAnExpiredCertificateIsAbsenceNotPresence is the interaction
// between the two halves, and the one easiest to get wrong.
//
// The connection presents the agent's OWN, CORRECT certificate — the bytes match
// the binding exactly — but the clock is outside its validity window, so
// WithClientCertificate attaches NOTHING and the gate sees a connection with no
// transport identity. It must be refused exactly like a connection that
// presented nothing, because that is what it is: crypto/tls proves possession of
// the leaf's private key and never looks at NotBefore or NotAfter, so an expired
// certificate completes the handshake identically to a fresh one, and expiry is
// the only automatic bound on a leaked client key.
//
// The certificate is real and valid; the SERVER'S CLOCK is moved past its
// NotAfter, which is the honest way to age one.
//
// MUTATION THAT KILLS IT ALONE: rewrite guard B's condition to the evadable
// `present && !containsFingerprint(...)` form (M2) — an ignored certificate is
// ABSENT, so the evadable form admits it. Deleting guard B's block does the same.
func TestCrossCheckAnExpiredCertificateIsAbsenceNotPresence(t *testing.T) {
	cert := ccCert(t, "agent-stale")
	future := cert.NotAfter.Add(24 * time.Hour)
	if !future.After(cert.NotAfter) {
		t.Fatalf("test setup: %v is not after NotAfter %v", future, cert.NotAfter)
	}
	bus := xcNewBus(t, future, nil)
	agentID, _ := bus.enrol("alpha", "k-1", cert)

	// Sanity: the binding really was recorded, or this test would be measuring
	// an unbound agent and would pass for the wrong reason.
	if live := bus.svc.LiveCertBindings(agentID); len(live) != 1 {
		t.Fatalf("test setup: agent %s holds %d live bindings, want 1", agentID, len(live))
	}

	rec := bus.begin(agentID, xcCerts(cert))
	xcWantRefused(t, rec, "session/begin presenting an EXPIRED copy of the bound certificate")
	if n := bus.svc.SessionCount(); n != 0 {
		t.Fatalf("the refusal minted %d session(s)", n)
	}
	if log := bus.logBuf.String(); !strings.Contains(log, "client_cert_presented=false") {
		t.Fatalf("an out-of-date certificate was treated as PRESENT; it attaches nothing and must be judged as absent. Log:\n%s", log)
	}
}

// ---------------------------------------------------------------------------
// THE CERTIFICATE ARM — guard A.
// ---------------------------------------------------------------------------

// TestCrossCheckAForeignCertificateOnAnUnboundAgentIsRefused is the case ONLY
// GUARD A COVERS, and it is the reason guard A is not redundant with guard B.
//
// Agent A enrolled over a connection carrying no certificate, so it has NO
// binding — the ordinary state of every agent enrolled before MTLS-BIND. The
// connection presents agent B's BOUND certificate and the credential names A.
// Guard B finds an EMPTY requirement for A and says nothing at all, so without
// guard A, B's certificate sails through on A's credential: literally the "a
// session token presented over a connection whose client certificate belongs to a
// DIFFERENT agent" sentence of invariant 11.
//
// MUTATION THAT KILLS IT ALONE: delete guard A's
// `case err == nil && owner != agentID` arm.
func TestCrossCheckAForeignCertificateOnAnUnboundAgentIsRefused(t *testing.T) {
	certB := ccCert(t, "agent-beta")
	bus := xcNewBus(t, ccValidNow(certB), nil)

	unbound, _ := bus.enrol("alpha", "k-1", nil)
	bound, _ := bus.enrol("beta", "k-2", certB)

	// The premise, asserted rather than assumed: A really has no requirement of
	// its own, so guard B is silent and only guard A can refuse this.
	if live := bus.svc.LiveCertBindings(unbound); len(live) != 0 {
		t.Fatalf("test setup: the unbound agent holds %d bindings, want 0; guard B would then be doing the work", len(live))
	}

	rec := bus.begin(unbound, xcCerts(certB))
	xcWantRefused(t, rec, "session/begin for an UNBOUND agent over another agent's bound certificate")
	if n := bus.svc.SessionCount(); n != 0 {
		t.Fatalf("the refusal minted %d session(s)", n)
	}
	// The LOG names the real owner, which is what an operator needs and what the
	// client is never told.
	log := bus.logBuf.String()
	if !strings.Contains(log, "certificate-belongs-to-another-agent") || !strings.Contains(log, bound) {
		t.Fatalf("the refusal did not report WHOSE certificate it was; log was:\n%s", log)
	}
	if strings.Contains(rec.Body.String(), bound) {
		t.Fatalf("the 403 body names the certificate's owner (%q), mapping a certificate an anonymous caller possesses to an agent id on this bus: %s", bound, rec.Body.String())
	}
}

// TestCrossCheckAMismatchedCertificateIsRefusedAndDiagnosedAsConfusion is the
// plain mismatch: agent A is bound, the connection presents agent B's
// certificate, and the credential names A.
//
// Both guards refuse this one, so the STATUS alone cannot tell them apart — which
// is exactly why this test also pins the LOG. Guard A runs first BECAUSE its
// refusal is the more specific one: it can name the agent the certificate really
// belongs to, and an operator diagnosing "somebody is using the wrong
// certificate" needs a different answer from "somebody sent no certificate". A
// request that trips both must be reported as the credential confusion it is.
//
// MUTATION THAT KILLS IT ALONE: delete guard A's
// `case err == nil && owner != agentID` arm. Guard B still answers 403, so a
// status-only assertion would stay GREEN — the log assertion is what makes this
// falsifiable.
func TestCrossCheckAMismatchedCertificateIsRefusedAndDiagnosedAsConfusion(t *testing.T) {
	certA := ccCert(t, "agent-alpha")
	certB := ccCert(t, "agent-beta")
	bus := xcNewBus(t, ccValidNow(certA), nil)

	alpha, _ := bus.enrol("alpha", "k-1", certA)
	beta, _ := bus.enrol("beta", "k-2", certB)

	rec := bus.begin(alpha, xcCerts(certB))
	xcWantRefused(t, rec, "session/begin naming alpha over beta's certificate")

	log := bus.logBuf.String()
	if !strings.Contains(log, "certificate-belongs-to-another-agent") {
		t.Fatalf("a certificate bound to ANOTHER agent was reported as a missing certificate; the two need different operator responses. Log:\n%s", log)
	}
	if !strings.Contains(log, "bound_agent_id="+beta) {
		t.Fatalf("the refusal line does not name the certificate's real owner %q; log was:\n%s", beta, log)
	}
	if !strings.Contains(log, buscert.FingerprintOf(certB).String()) {
		t.Fatalf("the refusal line does not carry the presented fingerprint, which is the value an operator correlates with a roster binding. Log:\n%s", log)
	}
}

// TestCrossCheckAnAmbiguousCertificateIsRefusedForEitherHolder is the case ONLY
// GUARD A CAN REFUSE, and the subtlest of the three.
//
// One fingerprint live on TWO agents resolves to NOBODY. Guard B would find it
// present in BOTH agents' live sets and admit EITHER, so one key holder would
// authenticate as two agents — the certificate would have stopped naming anybody,
// which is precisely the confusion invariant 11 exists to prevent. Guard A
// declines to guess; guard B structurally cannot.
//
// The state is built the ONLY way production can reach it: off disk, through
// recovery. See xcRecoveredRoster.
//
// MUTATION THAT KILLS IT ALONE: delete guard A's
// `case errors.Is(err, auth.ErrCertBindingAmbiguous)` arm. Guard B then admits
// BOTH agents with a 200 and a live token each.
func TestCrossCheckAnAmbiguousCertificateIsRefusedForEitherHolder(t *testing.T) {
	shared := ccCert(t, "agent-shared")
	alpha, beta := xcBusID+".alpha-1", xcBusID+".beta-1"
	roster := xcRecoveredRoster(t,
		xcRosterEntry(t, alpha, xcLive(shared)),
		xcRosterEntry(t, beta, xcLive(shared)),
	)
	// The premise: the fingerprint really does name nobody now.
	if owner, err := roster.AgentIDForCertFingerprint([32]byte(buscert.FingerprintOf(shared))); err == nil {
		t.Fatalf("test setup: the shared certificate still resolves, to %q; the ambiguity was not built", owner)
	}

	bus := xcNewBus(t, ccValidNow(shared), roster)
	for _, agentID := range []string{alpha, beta} {
		rec := bus.begin(agentID, xcCerts(shared))
		xcWantRefused(t, rec, "session/begin as "+agentID+" over an AMBIGUOUS certificate")
	}
	if n := bus.svc.SessionCount(); n != 0 {
		t.Fatalf("an ambiguous certificate minted %d session(s); it must authenticate NEITHER holder", n)
	}
	if log := bus.logBuf.String(); !strings.Contains(log, "certificate-ambiguous") {
		t.Fatalf("the ambiguity was not reported as such; log was:\n%s", log)
	}
}

// TestCrossCheckTheZeroFingerprintNeverMatchesAnything pins the OTHER direction
// of the zero-fingerprint rule (security gate, MTLS-CROSSCHECK), at the level
// where the rule actually lives.
//
// The sibling test below drives a REAL certificate at an agent whose only live
// binding is the zero value. This one asks the question the other way round: can
// the value a caller holds when there was NO certificate — the zero fingerprint —
// ever SATISFY a binding? It must not, or "this connection presented nothing"
// becomes "this connection satisfies the requirement", which is the fail-open
// LiveCertBindings' whole design intent is meant to prevent.
//
// WHY THIS IS A DIRECT UNIT TEST AND NOT ANOTHER HTTP ONE. Through the handler
// the property is currently unreachable: enforceCertBinding calls
// containsFingerprint only behind `present &&`, and Go short-circuits, so no
// request can deliver a zero fingerprint to the comparison. An HTTP-level test
// would therefore pass with the guard deleted and prove nothing. The exposure is
// that the safety rests on ONE TOKEN in a condition in another function — delete
// `present &&` as a plausible simplification and a no-certificate request starts
// matching a damaged zero binding. Testing containsFingerprint directly is what
// makes the guard, rather than the caller, the thing under test.
//
// MUTATION THAT KILLS IT ALONE: delete the `if fp == ([32]byte{})` early return
// from containsFingerprint.
func TestCrossCheckTheZeroFingerprintNeverMatchesAnything(t *testing.T) {
	var zero [32]byte
	real1 := [32]byte{1}

	if containsFingerprint([][32]byte{zero}, zero) {
		t.Fatal("the zero fingerprint MATCHED a live zero-fingerprint binding; it is the ABSENCE of a certificate, never a digest, so it must satisfy nothing")
	}
	if containsFingerprint([][32]byte{real1, zero}, zero) {
		t.Fatal("the zero fingerprint matched inside a mixed binding set; absence must never satisfy a requirement")
	}
	if containsFingerprint(nil, zero) {
		t.Fatal("the zero fingerprint matched an EMPTY binding set")
	}
	// The guard must not have broken ordinary matching.
	if !containsFingerprint([][32]byte{real1}, real1) {
		t.Fatal("a real fingerprint no longer matches its own binding; the zero guard has broken the positive path")
	}
}

// TestCrossCheckAZeroBindingRefusesAConnectionWithNoCertificate is the HTTP-level
// companion: an agent whose only live binding is the zero fingerprint is refused
// when the connection presents NOTHING, not merely when it presents a real
// certificate.
//
// It matters because absence and a zero binding are the two things most easily
// confused — both are "all zero bytes" somewhere — and an implementation that let
// them meet would admit exactly the request it should refuse.
//
// MUTATION THAT KILLS IT ALONE: delete guard B's `if` block (with guard B gone
// there is no live-binding requirement left to fail). Note it does NOT die to the
// containsFingerprint zero guard alone, which is why the direct unit test above
// exists.
func TestCrossCheckAZeroBindingRefusesAConnectionWithNoCertificate(t *testing.T) {
	cert := ccCert(t, "agent-clockref")
	roster := auth.NewMemoryRoster()
	agentID := xcBusID + ".rotted-2"
	if err := roster.Put(xcRosterEntry(t, agentID, auth.CertBinding{
		BoundAt: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
	})); err != nil {
		t.Fatalf("seeding a rotted entry: %v", err)
	}
	bus := xcNewBus(t, ccValidNow(cert), roster)

	rec := bus.begin(agentID, nil)
	xcWantRefused(t, rec, "session/begin for a zero-bound agent over NO certificate")
	if n := bus.svc.SessionCount(); n != 0 {
		t.Fatalf("an unsatisfiable agent obtained %d challenge(s)", n)
	}
}

// TestCrossCheckAZeroFingerprintBindingRefusesEvenARealCertificate is the httpapi
// consequence of internal/auth's fail-CLOSED choice about a rotted record.
//
// A live binding carrying the all-zero fingerprint can only come from a damaged
// or hand-edited durable record. It is INCLUDED in the agent's live set, which
// makes that agent UNSATISFIABLE — no certificate a client can present has an
// all-zero sha256 of its DER — so every request naming it is refused until an
// operator repairs the record. Loud, contained, fixable.
//
// FILTERING it would make the agent look UNBOUND and the requirement recorded
// against it would silently evaporate: the agent whose record ROTTED would get
// LESS enforcement than one whose record is intact. This test is the
// HTTP-observable consequence of that decision, and it is the one an operator
// would actually notice.
//
// MUTATION THAT KILLS IT ALONE: skip zero-fingerprint bindings in
// auth.Service.LiveCertBindings.
func TestCrossCheckAZeroFingerprintBindingRefusesEvenARealCertificate(t *testing.T) {
	cert := ccCert(t, "agent-real")
	roster := auth.NewMemoryRoster()
	agentID := xcBusID + ".rotted-1"
	if err := roster.Put(xcRosterEntry(t, agentID, auth.CertBinding{
		BoundAt: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
	})); err != nil {
		t.Fatalf("seeding a rotted entry: %v", err)
	}
	bus := xcNewBus(t, ccValidNow(cert), roster)

	rec := bus.begin(agentID, xcCerts(cert))
	xcWantRefused(t, rec, "session/begin for an agent whose only live binding is the zero fingerprint")
	if n := bus.svc.SessionCount(); n != 0 {
		t.Fatalf("an unsatisfiable agent obtained %d challenge(s)", n)
	}
}

// ---------------------------------------------------------------------------
// THE POSITIVE PATHS — every one of these must survive the gate, and each is a
// case a stricter rule would break.
// ---------------------------------------------------------------------------

// TestCrossCheckABoundAgentPresentingItsOwnCertificateIsAdmitted is the happy
// path, at TWO call sites, because a gate that refuses everything satisfies every
// negative test in this file.
//
// MUTATION THAT KILLS IT ALONE: invert guard B's condition by dropping the `!`
// — `if len(live) > 0 && (present && containsFingerprint(live, [32]byte(fp)))`
// — so the gate refuses exactly the certificate it should accept.
func TestCrossCheckABoundAgentPresentingItsOwnCertificateIsAdmitted(t *testing.T) {
	cert := ccCert(t, "agent-alpha")
	bus := xcNewBus(t, ccValidNow(cert), nil)
	agentID, priv := bus.enrol("alpha", "k-1", cert)

	// (1) The unauthenticated session route: a challenge IS issued.
	rec := bus.begin(agentID, xcCerts(cert))
	if rec.Code != http.StatusOK {
		t.Fatalf("session/begin over the agent's OWN certificate gave %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var begun SessionBeginResponseBody
	if err := json.Unmarshal(rec.Body.Bytes(), &begun); err != nil {
		t.Fatalf("decoding the 200 body: %v", err)
	}
	if begun.Token == "" {
		t.Fatalf("no token was issued: %s", rec.Body.String())
	}

	// (2) An AUTHENTICATED route, with a live token over the same certificate.
	// 404 is the pass here: the gate admitted the request and handleNotFound —
	// the only producer of a 404 on this build — ran. See the file header.
	token := bus.activeToken(agentID, priv)
	probe := xcReq(http.MethodGet, xcProbePath, "", xcCerts(cert))
	probe.Header.Set("Authorization", "Bearer "+token)
	got := bus.do(probe)
	if got.Code != http.StatusNotFound {
		t.Fatalf("an authenticated request over the bound certificate gave %d, want 404 (admitted, then no such route): %s", got.Code, got.Body.String())
	}
}

// TestCrossCheckAnUnboundAgentWithNoCertificateIsAdmitted pins THE MIGRATION
// ALLOWANCE, and it is a guard in both directions.
//
// Every agent enrolled before MTLS-BIND has no binding, and enrolment still
// accepts a connection presenting no certificate. Refusing those would be a flag
// day rather than a migration — it would lock every existing agent off the bus at
// once. The allowance is a NAMED GAP (a stolen token for such an agent is still
// replayable from a certificate-less connection) that closes agent by agent as
// they re-enrol.
//
// If a later change makes a certificate mandatory, this goes RED and that change
// has to argue for itself in DECISIONS.md instead of shipping as a silent
// lockout.
//
// MUTATION THAT KILLS IT ALONE: change guard B's condition to require a
// certificate regardless of the live set —
// `if !present || (len(live) > 0 && !containsFingerprint(live, [32]byte(fp)))` —
// which is the "just require mTLS everywhere" simplification this comment exists
// to prevent.
func TestCrossCheckAnUnboundAgentWithNoCertificateIsAdmitted(t *testing.T) {
	bus := xcNewBus(t, ccValidNow(ccCert(t, "agent-clockref")), nil)
	agentID, priv := bus.enrol("legacy", "k-1", nil)

	rec := bus.begin(agentID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session/begin for an UNBOUND agent over no certificate gave %d, want 200: %s", rec.Code, rec.Body.String())
	}

	token := bus.activeToken(agentID, priv)
	probe := xcReq(http.MethodGet, xcProbePath, "", nil)
	probe.Header.Set("Authorization", "Bearer "+token)
	if got := bus.do(probe); got.Code != http.StatusNotFound {
		t.Fatalf("an authenticated request from an UNBOUND agent over no certificate gave %d, want 404 (admitted): %s", got.Code, got.Body.String())
	}
}

// TestCrossCheckRotationAcceptsEitherLiveCertificate: invariant 11 has a rotation
// serve TWO certificates at once and NEVER require re-enrolment, so an agent
// holding two live bindings must be accepted presenting EITHER.
//
// "The newest binding wins" would refuse the OUTGOING certificate for the whole
// rollover — the one case the two-certificate rule exists to support — and it
// would fail silently, because the incoming certificate keeps working and only
// the clients that had not re-pinned yet break.
//
// MUTATION THAT KILLS IT ALONE: make containsFingerprint compare only the LAST
// element of live (the "newest wins" shortcut).
func TestCrossCheckRotationAcceptsEitherLiveCertificate(t *testing.T) {
	outgoing := ccCert(t, "agent-outgoing")
	incoming := ccCert(t, "agent-incoming")
	roster := auth.NewMemoryRoster()
	agentID := xcBusID + ".rotating-1"
	if err := roster.Put(xcRosterEntry(t, agentID, xcLive(outgoing), xcLive(incoming))); err != nil {
		t.Fatalf("seeding a rotating agent: %v", err)
	}
	// Judged at an instant valid for both certificates; asserted rather than
	// assumed, because a "now" outside one window would make half this test
	// measure expiry instead of rotation.
	now := ccValidNow(outgoing)
	if now.Before(incoming.NotBefore) || now.After(incoming.NotAfter) {
		t.Fatalf("test setup: %v is outside the incoming certificate's window", now)
	}
	bus := xcNewBus(t, now, roster)

	for _, cert := range []*x509.Certificate{outgoing, incoming} {
		rec := bus.begin(agentID, xcCerts(cert))
		if rec.Code != http.StatusOK {
			t.Fatalf("presenting %s during a rollover gave %d, want 200; ANY live binding satisfies the check, not the newest: %s",
				buscert.FingerprintOf(cert), rec.Code, rec.Body.String())
		}
	}
}

// TestCrossCheckARetiredBindingNeitherSatisfiesNorRequires pins both halves of
// retirement, which pull in opposite directions and are easy to get half right.
//
//   - A retired binding does NOT COUNT toward the requirement: an agent whose
//     only binding is retired is unconstrained again, exactly like one that never
//     had one. Counting it would keep refusing an agent that had legitimately
//     rotated away.
//   - A retired binding does NOT SATISFY the requirement either: presenting the
//     retired certificate to an agent that still has a live one is refused. A
//     retired binding names history, not a credential this bus will accept —
//     otherwise retirement would revoke nothing.
//
// MUTATION THAT KILLS IT ALONE: delete the `if b.RetiredAt != nil { continue }`
// arm from auth.Service.LiveCertBindings. Both subtests go red, in opposite
// directions, which is what makes the pair worth having.
func TestCrossCheckARetiredBindingNeitherSatisfiesNorRequires(t *testing.T) {
	old := ccCert(t, "agent-old")
	current := ccCert(t, "agent-current")

	t.Run("retired only: the agent is unconstrained again", func(t *testing.T) {
		roster := auth.NewMemoryRoster()
		agentID := xcBusID + ".retiredonly-1"
		if err := roster.Put(xcRosterEntry(t, agentID, xcRetired(old))); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		bus := xcNewBus(t, ccValidNow(old), roster)
		if rec := bus.begin(agentID, nil); rec.Code != http.StatusOK {
			t.Fatalf("an agent whose only binding is RETIRED was refused over no certificate: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a retired certificate does not satisfy a live requirement", func(t *testing.T) {
		roster := auth.NewMemoryRoster()
		agentID := xcBusID + ".rotated-1"
		if err := roster.Put(xcRosterEntry(t, agentID, xcRetired(old), xcLive(current))); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		bus := xcNewBus(t, ccValidNow(old), roster)
		rec := bus.begin(agentID, xcCerts(old))
		xcWantRefused(t, rec, "session/begin presenting a RETIRED certificate")
	})
}

// TestCrossCheckUnauthenticatedRoutesStillServeWithoutACertificate is
// NON-NEGOTIABLE: the container healthcheck depends on it, and so does
// pre-enrolment discovery.
//
// /healthz and /v1/info reach no principal — there is no agent id to check a
// certificate against — so authMiddleware returns before the cross-check runs.
// Enrolment is on the same allow-list for a stronger reason: it is where a
// binding is CREATED, so requiring one there would be circular.
//
// MUTATION THAT KILLS IT ALONE: delete the `next.ServeHTTP(w, r); return` from
// authMiddleware's IsUnauthenticatedRoute branch, so an allow-listed path falls
// through into the credentialled path (a 401 here, not a 403).
//
// A SECOND MUTATION THAT KILLS IT ALONE: add an enforceCertBinding call above
// that early return — the plausible shape of "apply the gate everywhere for
// consistency". It 403s every allow-listed route, taking the container
// healthcheck and pre-enrolment discovery down with it.
//
// AN EARLIER VERSION OF THIS COMMENT RECORDED THAT MUTATION AS HARMLESS, AND IT
// WAS WRONG TWICE OVER. It claimed the gate is "inert with an empty agent id" so
// the mutation changed nothing. The reviewer gate measured otherwise: even
// before this task added its empty-id arm, enforceCertBinding(w, r, "") was
// inert on GUARD B ONLY — with a resolvable client certificate on the
// connection, guard A already refused, because the certificate's owner is never
// "". So the "harmless" mutation would have 403'd /healthz for any prober
// presenting a bound client certificate, and this test missed it only because
// its probe presents none. enforceCertBinding now refuses an empty agent id
// outright, so the mutation is lethal unconditionally.
//
// It is recorded rather than deleted because the lesson is the point: a mutation
// observed green against ONE fixture is not a mutation that changes nothing, and
// writing "this is safe" from a single green run is how a gate acquires a
// property nobody has tested.
func TestCrossCheckUnauthenticatedRoutesStillServeWithoutACertificate(t *testing.T) {
	cert := ccCert(t, "agent-clockref")
	bus := xcNewBus(t, ccValidNow(cert), nil)
	// An agent WITH a binding exists on this bus, so a gate that ran here would
	// have something to refuse against rather than trivially passing.
	bus.enrol("alpha", "k-1", cert)

	for _, path := range []string{"/healthz", "/v1/info"} {
		rec := bus.do(xcReq(http.MethodGet, path, "", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s over a connection with NO client certificate gave %d, want 200; the container healthcheck and pre-enrolment discovery depend on this: %s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// THE THREE CALL SITES — each proved separately, because deleting any one of
// them leaves the other two green and a live hole behind.
// ---------------------------------------------------------------------------

// TestCrossCheckGatesAnAuthenticatedRoute: the gate runs in authMiddleware, AFTER
// Authenticate and BEFORE the principal is attached, so no downstream handler can
// ever see a principal the cross-check did not accept.
//
// The token here is genuine and ACTIVE — it authenticates perfectly. That is the
// point: this is the stolen-token case, replayed from a connection that does not
// hold the agent's certificate.
//
// MUTATION THAT KILLS IT ALONE: delete the `if !s.enforceCertBinding(...)` call
// from authMiddleware. The request then reaches the mux and answers 404.
func TestCrossCheckGatesAnAuthenticatedRoute(t *testing.T) {
	cert := ccCert(t, "agent-alpha")
	bus := xcNewBus(t, ccValidNow(cert), nil)
	agentID, priv := bus.enrol("alpha", "k-1", cert)
	token := bus.activeToken(agentID, priv)

	// The premise: the token DOES authenticate, so a refusal can only come from
	// the cross-check and not from the bearer path.
	if _, err := bus.svc.Authenticate(token); err != nil {
		t.Fatalf("test setup: the token does not authenticate: %v", err)
	}

	probe := xcReq(http.MethodGet, xcProbePath, "", nil)
	probe.Header.Set("Authorization", "Bearer "+token)
	rec := bus.do(probe)
	xcWantRefused(t, rec, "an authenticated request replayed over no certificate")
	if rec.Code == http.StatusNotFound {
		t.Fatal("the request reached the mux; the gate must refuse before the principal is attached")
	}
}

// TestCrossCheckGatesSessionBegin: the gate runs BEFORE a challenge is minted.
//
// body.AgentID there is UNTRUSTED and UNVALIDATED — whatever the client put in
// the body — and that is fine, because the question is not "is this caller that
// agent" (the challenge proves that) but "may a credential for the agent this
// request NAMES be issued over THIS connection". Running it first means no token
// is minted for a mismatched connection at all: a challenge is server state with
// a lifetime, and an unauthenticated caller must not be able to create some for an
// agent whose certificate it does not hold.
//
// MUTATION THAT KILLS IT ALONE: delete the `if !s.enforceCertBinding(...)` call
// from handleSessionBegin. The route then answers 200 with a live token.
func TestCrossCheckGatesSessionBegin(t *testing.T) {
	cert := ccCert(t, "agent-alpha")
	bus := xcNewBus(t, ccValidNow(cert), nil)
	agentID, _ := bus.enrol("alpha", "k-1", cert)

	rec := bus.begin(agentID, nil)
	xcWantRefused(t, rec, "session/begin over no certificate for a bound agent")
	if n := bus.svc.SessionCount(); n != 0 {
		t.Fatalf("the refused begin minted %d session(s); the gate must run BEFORE BeginSession", n)
	}
}

// TestCrossCheckGatesSessionComplete: the gate runs on the SERVER-SIDE agent id
// out of the completed session — never a value from the request body, which
// carries no agent id at all and must never grow one, since a client-supplied id
// here would let a caller choose which binding it is measured against.
//
// It necessarily runs AFTER CompleteSession, because the agent is simply not
// knowable before: the request carries a token and a signature, and only
// resolving the token yields the agent. The session is therefore left ACTIVATED
// even when this refuses — acceptable because completing a challenge requires a
// valid signature under the agent's OWN private key (so the caller IS the agent),
// and because every later request bearing that token meets the SAME check in
// authMiddleware, so an activated-but-refused token authorises nothing anywhere.
// That residue is deliberately NOT asserted here: it is a documented consequence
// owned by AUTH-4, not a property this gate promises.
//
// MUTATION THAT KILLS IT ALONE: delete the `if !s.enforceCertBinding(...)` call
// from handleSessionComplete. The route then answers 200 with the session body.
func TestCrossCheckGatesSessionComplete(t *testing.T) {
	cert := ccCert(t, "agent-alpha")
	bus := xcNewBus(t, ccValidNow(cert), nil)
	agentID, priv := bus.enrol("alpha", "k-1", cert)

	// The challenge is obtained through the SERVICE (see activeToken for why),
	// then completed over HTTP on a connection carrying NO certificate.
	ch, err := bus.svc.BeginSession(agentID)
	if err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	sig := ed25519.Sign(priv, []byte(auth.SessionSigningContext+ch.Token))
	body, err := json.Marshal(SessionCompleteRequestBody{
		Token:     ch.Token,
		Signature: encodeBase64(sig),
	})
	if err != nil {
		t.Fatalf("marshalling the session/complete body: %v", err)
	}

	rec := bus.do(xcReq(http.MethodPost, RouteSessionComplete, string(body), nil))
	xcWantRefused(t, rec, "session/complete over no certificate for a bound agent")
	if strings.Contains(rec.Body.String(), "expires_at") {
		t.Fatalf("the refusal returned a session body: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// WHAT A REFUSED CALLER, AND THE LOG, ARE ALLOWED TO LEARN.
// ---------------------------------------------------------------------------

// TestCrossCheckRefusalsAreByteIdenticalAcrossCases: the refusal is ONE fixed
// string for every case, and the responses are byte-identical.
//
// Distinguishing them would tell a caller WHICH HALF of the pair was wrong — in
// particular it would separate "this agent has a binding and you do not hold it"
// from "this agent has none", which is a free read of the roster's migration
// state and a map of exactly which agents are still replay-able. The LOG says
// precisely which guard fired; the client learns nothing.
//
// MUTATION THAT KILLS IT ALONE: give any one of the refusal sites its own reason
// string — e.g. return "this client certificate is bound to another agent" from
// guard A's first arm.
func TestCrossCheckRefusalsAreByteIdenticalAcrossCases(t *testing.T) {
	type refusal struct {
		name string
		rec  *httptest.ResponseRecorder
		// secret is a value that must NOT appear in the response: the agent id
		// the certificate is really bound to.
		secret string
	}
	var got []refusal

	// (1) MISMATCH.
	{
		certA, certB := ccCert(t, "agent-alpha"), ccCert(t, "agent-beta")
		bus := xcNewBus(t, ccValidNow(certA), nil)
		alpha, _ := bus.enrol("alpha", "k-1", certA)
		beta, _ := bus.enrol("beta", "k-2", certB)
		got = append(got, refusal{"mismatch", bus.begin(alpha, xcCerts(certB)), beta})
	}
	// (2) AMBIGUOUS.
	{
		shared := ccCert(t, "agent-shared")
		alpha, beta := xcBusID+".alpha-1", xcBusID+".beta-1"
		roster := xcRecoveredRoster(t,
			xcRosterEntry(t, alpha, xcLive(shared)),
			xcRosterEntry(t, beta, xcLive(shared)),
		)
		bus := xcNewBus(t, ccValidNow(shared), roster)
		got = append(got, refusal{"ambiguous", bus.begin(alpha, xcCerts(shared)), beta})
	}
	// (3) ABSENCE.
	{
		cert := ccCert(t, "agent-alpha")
		bus := xcNewBus(t, ccValidNow(cert), nil)
		agentID, _ := bus.enrol("alpha", "k-1", cert)
		got = append(got, refusal{"absence", bus.begin(agentID, nil), agentID})
	}

	for _, r := range got {
		xcWantRefused(t, r.rec, r.name)
		if strings.Contains(r.rec.Body.String(), r.secret) {
			t.Fatalf("%s: the refusal body names %q: %s", r.name, r.secret, r.rec.Body.String())
		}
	}
	want := got[0].rec.Body.String()
	for _, r := range got[1:] {
		if body := r.rec.Body.String(); body != want {
			t.Fatalf("the %s refusal body is %q but the %s refusal body is %q; they must be byte-identical, or the response becomes an oracle for WHICH half of the pair was wrong",
				r.name, body, got[0].name, want)
		}
	}
}

// TestCrossCheckRefusalNeverLogsTheBearerToken: the token is a live credential
// and appears NOWHERE on this path — not truncated, not hashed, not inside an
// error.
//
// The token used here is a REAL one, minted by internal/auth, so the search is
// for a value that could genuinely be found if it leaked.
//
// MUTATION THAT KILLS IT ALONE: add the Authorization header (or the token) to
// crossCheckLogFields — the plausible version being `"authorization",
// r.Header.Get("Authorization")` "for debugging".
func TestCrossCheckRefusalNeverLogsTheBearerToken(t *testing.T) {
	cert := ccCert(t, "agent-alpha")
	bus := xcNewBus(t, ccValidNow(cert), nil)
	agentID, priv := bus.enrol("alpha", "k-1", cert)
	token := bus.activeToken(agentID, priv)

	probe := xcReq(http.MethodGet, xcProbePath, "", nil)
	probe.Header.Set("Authorization", "Bearer "+token)
	xcWantRefused(t, bus.do(probe), "an authenticated request replayed over no certificate")

	log := bus.logBuf.String()
	if len(token) < 20 {
		t.Fatalf("test setup: the token %q is too short for this search to mean anything", token)
	}
	if strings.Contains(log, token) {
		t.Fatalf("the session token appears in the log:\n%s", log)
	}
	// And not a PREFIX of it either: a truncated credential is still a
	// credential-shaped secret in a log file, and "just log the first few
	// characters" is how this rule dies.
	if strings.Contains(log, token[:16]) {
		t.Fatalf("a prefix of the session token appears in the log:\n%s", log)
	}
	// The test would be vacuous if the refusal had logged nothing at all.
	if !strings.Contains(log, "required-certificate-not-presented") {
		t.Fatalf("the refusal logged nothing, so the absence of the token proves nothing; log was:\n%s", log)
	}
}

// TestCrossCheckLogsAnOversizedAgentIDByLengthOnly is the log-amplification
// guard, and it is about VOLUME, not escaping.
//
// logging.writeValue already quotes every byte it writes, so the id is SAFE to
// write. The problem is WHO CHOOSES THE BYTES AND HOW MANY: POST
// /v1/session/begin is unauthenticated, this server rate-limits nothing, and
// MaxAuthRequestBytes (8 KiB) lets an anonymous caller put a kilobyte of chosen
// bytes — internal/logging's per-value cap — into a WARN-level record on every
// cheap request. So a MALFORMED id is logged by LENGTH only, and deliberately not
// as a truncated prefix: a prefix of an attacker-chosen id is still
// attacker-chosen bytes, and it invites the next reader to "just log a bit more".
// This is the discipline inviteIDLogFields already applies, for the same reason.
//
// MUTATION THAT KILLS IT ALONE: make agentIDLogFields' malformed-id branch
// return `{"agent_id", agentID}` — i.e. log the client-chosen bytes in both
// branches.
func TestCrossCheckLogsAnOversizedAgentIDByLengthOnly(t *testing.T) {
	cert := ccCert(t, "agent-beta")
	bus := xcNewBus(t, ccValidNow(cert), nil)
	bus.enrol("beta", "k-1", cert)

	// 4000 bytes of a distinctive, greppable byte, inside MaxAuthRequestBytes so
	// the body is accepted and the gate is actually reached. Presenting beta's
	// BOUND certificate is what makes guard A fire on an id that names nobody.
	junk := strings.Repeat("Z", 4000)
	rec := bus.begin(junk, xcCerts(cert))
	xcWantRefused(t, rec, "session/begin with a 4000-byte junk agent_id over a bound certificate")

	log := bus.logBuf.String()
	if !strings.Contains(log, "agent_id_len=4000") {
		t.Fatalf("the refusal did not record the id's LENGTH; log was:\n%s", log)
	}
	// Not the bytes — not in the agent_id field, not smuggled back through an
	// "err" field, not anywhere in the record. 200 characters is far below
	// internal/logging's per-value cap, so a truncated echo would still be caught.
	if strings.Contains(log, strings.Repeat("Z", 200)) {
		t.Fatalf("the client-chosen id bytes reached the log; log was:\n%s", log)
	}
	if strings.Contains(rec.Body.String(), "ZZZ") {
		t.Fatalf("the refusal echoed the client-chosen id back: %s", rec.Body.String())
	}
	if n := bus.svc.SessionCount(); n != 0 {
		t.Fatalf("a junk agent id minted %d session(s)", n)
	}
}

// TestCrossCheckRefusesWhenNoAgentIsNamed pins the OTHER fail-closed arm
// (reviewer gate, MTLS-CROSSCHECK): an EMPTY agent id is refused outright.
//
// An empty id is not "an agent that holds no binding", it is NO AGENT AT ALL. A
// gate asked to check a credential against nobody has been asked a question it
// cannot answer, which is the same case as a server with no auth service.
//
// WHY IT IS PINNED HERE RATHER THAN LEFT TO THE CALLERS. Before this arm existed
// the function was safe only because of facts OUTSIDE it — two call sites pass a
// server-minted id, and /v1/session/begin's empty value was refused downstream by
// BeginSession's 404. That is not a property of the gate, it is a property of
// today's three callers, and it evaporates silently when a fourth is added. The
// partial cover it had was genuinely partial: with a resolvable certificate on
// the connection guard A already refused (an owner is never ""), but with NO
// certificate guard B had no live set to require and ADMITTED.
//
// So the direct call below deliberately presents NO certificate — that is the
// arm that was open. A version presenting a bound certificate would pass even
// with this guard deleted, because guard A would catch it, and would therefore
// prove nothing.
//
// MUTATION THAT KILLS IT ALONE: delete the `if agentID == ""` block from
// enforceCertBinding.
func TestCrossCheckRefusesWhenNoAgentIsNamed(t *testing.T) {
	cert := ccCert(t, "agent-unnamed")
	bus := xcNewBus(t, ccValidNow(cert), nil)

	// (a) The direct call, over a connection presenting NO certificate: the arm
	// guard A cannot cover.
	rec := httptest.NewRecorder()
	if bus.srv.enforceCertBinding(rec, xcReq(http.MethodGet, xcProbePath, "", nil), "") {
		t.Fatal("enforceCertBinding ADMITTED a request naming NO agent over a connection with no certificate; there is nothing to check the certificate against, so it must fail closed")
	}
	xcWantRefused(t, rec, "a request naming no agent at all")
	if log := bus.logBuf.String(); !strings.Contains(log, "no-agent-named") {
		t.Fatalf("the refusal did not name the no-agent-named guard; log was:\n%s", log)
	}

	// (b) Over HTTP, through the REAL chain: POST /v1/session/begin with an empty
	// agent_id is 403 and mints NOTHING. It was a 404 before this arm existed
	// (BeginSession's own refusal); the point of the change is that the gate now
	// refuses it FIRST, on its own authority.
	got := bus.begin("", nil)
	xcWantRefused(t, got, `session/begin with an empty agent_id`)
	if n := bus.svc.SessionCount(); n != 0 {
		t.Fatalf("a refused session/begin left %d session(s) behind; a refusal must mint nothing", n)
	}
}

// TestCrossCheckRefusesWhenTheServerHasNoAuthService is the FAIL-CLOSED arm.
//
// A server that cannot resolve a binding cannot prove one is satisfied, and "we
// could not check, so we allowed it" is how a control disappears under a
// misconfiguration. It is called directly because a server built with no Auth
// registers none of the credential routes and authMiddleware 401s everything
// else, so there is no request that reaches this branch through the mux — which
// is exactly why the branch needs its own guard rather than being assumed
// unreachable.
//
// MUTATION THAT KILLS IT ALONE: make the `s.auth == nil` branch `return true`.
func TestCrossCheckRefusesWhenTheServerHasNoAuthService(t *testing.T) {
	var logBuf bytes.Buffer
	srv := New(Options{
		Identity: StaticIdentity(xcBusID),
		Logger:   logging.New(&logBuf, logging.LevelDebug),
	})
	if srv.Auth() != nil {
		t.Fatal("test setup: this server was built WITH an auth service")
	}

	rec := httptest.NewRecorder()
	req := xcReq(http.MethodGet, xcProbePath, "", nil)
	if srv.enforceCertBinding(rec, req, xcBusID+".alpha-1") {
		t.Fatal("a server with no auth service ADMITTED the request; it cannot resolve a binding, so it cannot prove one is satisfied and must fail closed")
	}
	xcWantRefused(t, rec, "a request to a server with no auth service")
	if log := logBuf.String(); !strings.Contains(log, "no-auth-service") {
		t.Fatalf("the operator misconfiguration was not reported; log was:\n%s", log)
	}
}

// TestCrossCheckEveryMutationLineNamesItsOwnTest is the MECHANICAL guard against
// a defect class this task hit THREE SEPARATE TIMES, each caught by hand.
//
// # THE DEFECT
//
// Inserting a test between an existing doc comment and its func, with no blank
// line, silently REATTACHES that comment to the new function. Go's parser joins
// adjacent comment lines into one block and binds it to whatever declaration
// follows. Nothing warns: it compiles, gofmt is happy, vet is happy, and every
// test still passes. What breaks is the EVIDENCE — the block's
// "MUTATION THAT KILLS IT ALONE" line now sits above a test it does not kill,
// and the test it does kill is left with no documentation at all.
//
// That matters more here than anywhere else in the tree, because on this task
// the mutation line is the ONLY record of how each guard was proven able to
// fail. A false one is worse than a missing one: it invites the next reader to
// trust a guard nobody has actually broken. Reviewers caught it at the
// no-auth-service block, then again at the zero-fingerprint block. A third
// manual catch is a process telling you to automate it.
//
// # WHAT IT ENFORCES
//
// For every TestCrossCheck* function in this task's two test files:
//
//  1. it HAS a doc comment;
//  2. the comment's first line NAMES THAT FUNCTION — this is the check that
//     detects reattachment, because a stolen block names the other test;
//  3. the comment carries at least one MUTATION THAT KILLS IT ALONE line.
//
// It reads the files with go/parser rather than grepping, because the bug IS
// the parser's attachment rule: a grep sees two comment blocks where the
// compiler sees one, so a textual check cannot detect it by construction.
//
// SEVERAL mutation lines are allowed. TestCrossCheckUnauthenticatedRoutesStill
// ServeWithoutACertificate deliberately documents two, both its own.
func TestCrossCheckEveryMutationLineNamesItsOwnTest(t *testing.T) {
	const marker = "MUTATION THAT KILLS IT ALONE"

	for _, path := range []string{
		"crosscheck_mtlscrosscheck_test.go",
		filepath.Join("..", "auth", "crosscheck_mtlscrosscheck_test.go"),
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}

		seen := 0
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "TestCrossCheck") {
				continue
			}
			seen++

			if fn.Doc == nil {
				t.Errorf("%s: %s has NO doc comment. Every guard on this task must record the mutation that proves it can fail; a test with no doc is usually one whose block was stolen by the function below it.", path, fn.Name.Name)
				continue
			}

			doc := fn.Doc.Text()
			var first string
			for _, line := range strings.Split(doc, "\n") {
				if strings.TrimSpace(line) != "" {
					first = strings.TrimSpace(line)
					break
				}
			}
			if !strings.HasPrefix(first, fn.Name.Name) {
				t.Errorf("%s: the doc comment attached to %s begins %q, which names a DIFFERENT test.\n"+
					"That is comment REATTACHMENT: a block was inserted above this func with no blank line, so Go bound the previous test's comment here.\n"+
					"Its MUTATION THAT KILLS IT ALONE line now describes a mutation that does NOT kill this test, and the test it does kill has been left undocumented.\n"+
					"FIX: move that block back above its own func and separate the two with a blank line.",
					path, fn.Name.Name, truncateForMsg(first))
				continue
			}

			if !strings.Contains(doc, marker) {
				t.Errorf("%s: %s has a doc comment but no %q line. On this task every guard must name the single edit that turns it red, and that line must have been OBSERVED failing -- a guard nobody has watched fail is not evidence.", path, fn.Name.Name, marker)
			}
		}

		// A guard that inspects nothing passes vacuously, which is the exact
		// failure mode CLAUDE.md warns about for -run patterns. Pin that it
		// actually found tests.
		if seen == 0 {
			t.Fatalf("%s: the guard found NO TestCrossCheck* functions, so it asserted nothing. Either the file moved or the prefix changed; a guard that inspects nothing is not a guard.", path)
		}
	}
}

// truncateForMsg bounds a quoted line in a failure message. The input is our own
// source, not attacker input, so this is about readability rather than safety.
func truncateForMsg(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
