package httpapi_test

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/wal"
)

func TestPreTLSMigrationBootstrapsFingerprintAndClientCertificate(t *testing.T) {
	material, err := buscert.LoadOrCreate(t.TempDir(), buscert.Options{BusID: "agent-migrate-cert"})
	if err != nil {
		t.Fatalf("buscert.LoadOrCreate: %v", err)
	}
	cert := material.Certificate()
	fp := buscert.FingerprintOf(cert)
	now := cert.NotBefore.Add(time.Hour)
	busID := "bus-migrate-test"
	agentID := busID + ".legacy-1"
	pub, priv, _ := newAuthKeypair(t)
	roster := auth.NewMemoryRoster()
	if err := roster.Put(auth.RosterEntry{
		AgentID:       agentID,
		Name:          "legacy",
		AuthPublicKey: pub,
		Epoch:         now,
		EnrolledAt:    now,
	}); err != nil {
		t.Fatalf("preloading legacy roster entry: %v", err)
	}
	minter, err := ids.NewAgentIDMinter(busID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("ids.NewAgentIDMinter: %v", err)
	}
	svc, err := auth.NewService(auth.Options{
		Minter: minter,
		Roster: roster,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}
	srv := httpapi.New(httpapi.Options{
		Identity: httpapi.StaticIdentity(busID),
		Auth:     svc,
		Now:      func() time.Time { return now },
	})

	ch, err := svc.BeginSession(agentID)
	if err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	if _, err := svc.CompleteSession(ch.Token, ed25519.Sign(priv, []byte(auth.SessionSigningContext+ch.Token))); err != nil {
		t.Fatalf("CompleteSession: %v", err)
	}

	sig := ed25519.Sign(priv, auth.ClientCertificateBootstrapSigningBytes(ch.Token, "migrate-key", [32]byte(fp)))
	bodyText := `{"idempotency_key":"migrate-key","signature":"` + base64.StdEncoding.EncodeToString(sig) + `"}`
	req := httptest.NewRequest(http.MethodPost, httpapi.RouteClientCertBootstrap, strings.NewReader(bodyText))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ch.Token)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["agent_id"] != agentID {
		t.Fatalf("agent_id = %#v, want %q", body["agent_id"], agentID)
	}
	if body["client_cert_fingerprint"] != fp.String() {
		t.Fatalf("client_cert_fingerprint = %#v, want %q", body["client_cert_fingerprint"], fp.String())
	}

	entry, ok := roster.Get(agentID)
	if !ok {
		t.Fatalf("legacy agent %q disappeared from roster", agentID)
	}
	if entry.AgentID != agentID || entry.Name != "legacy" {
		t.Fatalf("identity changed after migration: %+v", entry)
	}
	if entry.InviteID != "" {
		t.Fatalf("InviteID = %q, want empty legacy value; bootstrap must not spend an invite", entry.InviteID)
	}
	if len(entry.CertBindings) != 1 {
		t.Fatalf("CertBindings len = %d, want 1", len(entry.CertBindings))
	}
	if got := entry.CertBindings[0].Fingerprint; got != [32]byte(fp) {
		t.Fatalf("stored fingerprint = %x, want %s", got, fp)
	}
}

func TestPreTLSMigrationRequiresSessionAndCertificateAndReplaysRetry(t *testing.T) {
	cert := mtlsMigrationCert(t, "agent-bootstrap")
	h := newMTLSMigrationHarness(t, auth.NewMemoryRoster(), cert)

	missingSession := h.bootstrap("", cert, "need-session")
	if missingSession.Code != http.StatusUnauthorized {
		t.Fatalf("bootstrap without a bearer session = %d, want 401; body %s", missingSession.Code, missingSession.Body.String())
	}
	h.wantNoBindings(t)

	missingCert := h.bootstrap(h.token, nil, "need-cert")
	if missingCert.Code != http.StatusForbidden {
		t.Fatalf("bootstrap without a client certificate = %d, want 403; body %s", missingCert.Code, missingCert.Body.String())
	}
	h.wantNoBindings(t)

	attacker := mtlsMigrationCert(t, "attacker-cert")
	_, attackerPriv, _ := newAuthKeypair(t)
	stolen := h.bootstrapSigned(h.token, attacker, "stolen-session", attackerPriv)
	if stolen.Code != http.StatusForbidden {
		t.Fatalf("bootstrap with stolen bearer and attacker auth proof = %d, want 403; body %s", stolen.Code, stolen.Body.String())
	}
	h.wantNoBindings(t)

	first := h.bootstrap(h.token, cert, "retry-key")
	if first.Code != http.StatusOK {
		t.Fatalf("first bootstrap = %d, want 200; body %s", first.Code, first.Body.String())
	}
	firstBody := decodeBody(t, first)
	if firstBody["already_bound"] != false {
		t.Fatalf("first already_bound = %#v, want false", firstBody["already_bound"])
	}

	retry := h.bootstrap(h.token, cert, "retry-key")
	if retry.Code != http.StatusOK {
		t.Fatalf("retry bootstrap = %d, want 200; body %s", retry.Code, retry.Body.String())
	}
	if got := retry.Result().Header.Get(httpapi.IdempotencyReplayedHeader); got != "true" {
		t.Fatalf("%s = %q on retry, want \"true\": the idempotency key must be remembered, not only the roster state", httpapi.IdempotencyReplayedHeader, got)
	}
	if retry.Body.String() != first.Body.String() {
		t.Fatalf("retry body was not the original response.\nfirst: %s\nretry: %s", first.Body.String(), retry.Body.String())
	}
	h.wantOneBinding(t, cert)
}

func TestPreTLSMigrationRefusesConflictingCertificateAfterBinding(t *testing.T) {
	cert := mtlsMigrationCert(t, "agent-bound")
	other := mtlsMigrationCert(t, "agent-other")
	h := newMTLSMigrationHarness(t, auth.NewMemoryRoster(), cert)

	first := h.bootstrap(h.token, cert, "bind-once")
	if first.Code != http.StatusOK {
		t.Fatalf("first bootstrap = %d, want 200; body %s", first.Code, first.Body.String())
	}

	conflict := h.bootstrap(h.token, other, "bind-once")
	if conflict.Code != http.StatusForbidden {
		t.Fatalf("bootstrap with a different certificate after binding = %d, want 403 from the mTLS/session cross-check; body %s", conflict.Code, conflict.Body.String())
	}
	if got := conflict.Result().Header.Get("Connection"); strings.EqualFold(got, "close") {
		t.Fatalf("conflicting bootstrap closed the connection; same-key/different-cert must reject without disconnect")
	}
	h.wantOneBinding(t, cert)
}

func TestPreTLSMigrationBindingSurvivesWALRosterRestart(t *testing.T) {
	cert := mtlsMigrationCert(t, "agent-durable")
	dir := t.TempDir()
	roster, log := openMigrationWALRoster(t, dir)
	h := newMTLSMigrationHarness(t, roster, cert)

	rec := h.bootstrap(h.token, cert, "durable-bind")
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap through WALRoster = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if err := log.Close(); err != nil {
		t.Fatalf("closing WAL before restart: %v", err)
	}

	recovered, recoveredLog := openMigrationWALRoster(t, dir)
	defer recoveredLog.Close()
	entry, ok := recovered.Get(h.agentID)
	if !ok {
		t.Fatalf("agent %q did not recover after restart", h.agentID)
	}
	if len(entry.CertBindings) != 1 {
		t.Fatalf("recovered CertBindings len = %d, want 1", len(entry.CertBindings))
	}
	fp := buscert.FingerprintOf(cert)
	if got := entry.CertBindings[0].Fingerprint; got != [32]byte(fp) {
		t.Fatalf("recovered fingerprint = %x, want %s", got, fp)
	}
	recoveredSvc, err := auth.NewService(auth.Options{
		Minter: h.minter,
		Roster: recovered,
		Now:    func() time.Time { return h.now },
	})
	if err != nil {
		t.Fatalf("auth.NewService after restart: %v", err)
	}
	if got, err := recoveredSvc.AgentIDForClientCertificate([32]byte(fp)); err != nil || got != h.agentID {
		t.Fatalf("AgentIDForClientCertificate after restart = (%q, %v), want (%q, nil)", got, err, h.agentID)
	}
	ch, err := recoveredSvc.BeginSession(h.agentID)
	if err != nil {
		t.Fatalf("BeginSession after restart: %v", err)
	}
	if _, err := recoveredSvc.CompleteSession(ch.Token, ed25519.Sign(h.priv, []byte(auth.SessionSigningContext+ch.Token))); err != nil {
		t.Fatalf("CompleteSession after restart: %v", err)
	}
	recoveredSrv := httpapi.New(httpapi.Options{Identity: httpapi.StaticIdentity("bus-migrate-test"), Auth: recoveredSvc, Now: func() time.Time { return h.now }})
	sig := ed25519.Sign(h.priv, auth.ClientCertificateBootstrapSigningBytes(ch.Token, "durable-bind", [32]byte(fp)))
	req := httptest.NewRequest(http.MethodPost, httpapi.RouteClientCertBootstrap, strings.NewReader(`{"idempotency_key":"durable-bind","signature":"`+base64.StdEncoding.EncodeToString(sig)+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ch.Token)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	retry := httptest.NewRecorder()
	recoveredSrv.ServeHTTP(retry, req)
	if retry.Code != http.StatusOK {
		t.Fatalf("recovered idempotent retry = %d, want 200; body %s", retry.Code, retry.Body.String())
	}
	if got := retry.Result().Header.Get(httpapi.IdempotencyReplayedHeader); got != "true" {
		t.Fatalf("recovered retry %s = %q, want true", httpapi.IdempotencyReplayedHeader, got)
	}
}

type mtlsMigrationHarness struct {
	srv     *httpapi.Server
	roster  auth.Roster
	minter  *ids.AgentIDMinter
	agentID string
	token   string
	priv    ed25519.PrivateKey
	now     time.Time
}

func newMTLSMigrationHarness(t *testing.T, roster auth.Roster, cert *x509.Certificate) mtlsMigrationHarness {
	t.Helper()
	now := cert.NotBefore.Add(time.Hour)
	busID := "bus-migrate-test"
	agentID := busID + ".legacy-1"
	pub, priv, _ := newAuthKeypair(t)
	if err := roster.Put(auth.RosterEntry{
		AgentID:       agentID,
		Name:          "legacy",
		AuthPublicKey: pub,
		Epoch:         now,
		EnrolledAt:    now,
	}); err != nil {
		t.Fatalf("preloading legacy roster entry: %v", err)
	}
	minter, err := ids.NewAgentIDMinter(busID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("ids.NewAgentIDMinter: %v", err)
	}
	svc, err := auth.NewService(auth.Options{
		Minter: minter,
		Roster: roster,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}
	ch, err := svc.BeginSession(agentID)
	if err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	if _, err := svc.CompleteSession(ch.Token, ed25519.Sign(priv, []byte(auth.SessionSigningContext+ch.Token))); err != nil {
		t.Fatalf("CompleteSession: %v", err)
	}
	srv := httpapi.New(httpapi.Options{
		Identity: httpapi.StaticIdentity(busID),
		Auth:     svc,
		Now:      func() time.Time { return now },
	})
	return mtlsMigrationHarness{srv: srv, roster: roster, minter: minter, agentID: agentID, token: ch.Token, priv: priv, now: now}
}

func (h mtlsMigrationHarness) bootstrap(token string, cert *x509.Certificate, idemKey string) *httptest.ResponseRecorder {
	return h.bootstrapSigned(token, cert, idemKey, h.priv)
}

func (h mtlsMigrationHarness) bootstrapSigned(token string, cert *x509.Certificate, idemKey string, priv ed25519.PrivateKey) *httptest.ResponseRecorder {
	var fp [32]byte
	if cert != nil {
		f := buscert.FingerprintOf(cert)
		fp = [32]byte(f)
	}
	sig := ed25519.Sign(priv, auth.ClientCertificateBootstrapSigningBytes(token, idemKey, fp))
	body := `{"idempotency_key":"` + idemKey + `","signature":"` + base64.StdEncoding.EncodeToString(sig) + `"}`
	req := httptest.NewRequest(http.MethodPost, httpapi.RouteClientCertBootstrap, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if cert != nil {
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	}
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	return rec
}

func (h mtlsMigrationHarness) wantNoBindings(t *testing.T) {
	t.Helper()
	entry, ok := h.roster.Get(h.agentID)
	if !ok {
		t.Fatalf("test setup lost the legacy agent %q", h.agentID)
	}
	if len(entry.CertBindings) != 0 {
		t.Fatalf("bootstrap refusal recorded %d certificate binding(s), want 0", len(entry.CertBindings))
	}
}

func (h mtlsMigrationHarness) wantOneBinding(t *testing.T, cert *x509.Certificate) {
	t.Helper()
	entry, ok := h.roster.Get(h.agentID)
	if !ok {
		t.Fatalf("test setup lost the legacy agent %q", h.agentID)
	}
	fp := buscert.FingerprintOf(cert)
	if len(entry.CertBindings) != 1 {
		t.Fatalf("CertBindings len = %d, want 1", len(entry.CertBindings))
	}
	if got := entry.CertBindings[0].Fingerprint; got != [32]byte(fp) {
		t.Fatalf("stored fingerprint = %x, want %s", got, fp)
	}
}

func mtlsMigrationCert(t *testing.T, id string) *x509.Certificate {
	t.Helper()
	material, err := buscert.LoadOrCreate(t.TempDir(), buscert.Options{BusID: id})
	if err != nil {
		t.Fatalf("buscert.LoadOrCreate: %v", err)
	}
	return material.Certificate()
}

func openMigrationWALRoster(t *testing.T, dir string) (*auth.WALRoster, *wal.Log) {
	t.Helper()
	roster := auth.NewWALRoster(nil)
	log, err := wal.Open(wal.LogOptions{Dir: dir, Applier: roster})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	if err := roster.Attach(log); err != nil {
		log.Close()
		t.Fatalf("Attach: %v", err)
	}
	return roster, log
}
