package main

// RELAY-45's LIVE evidence: a real TLS handshake, a real client certificate, a
// real durable binding, and the gate in between.
//
// The two layers below this one are proved on their own — internal/relay proves
// the durable binding and its revocation, internal/httpapi proves the gate's
// refusals — and neither of them touches a socket. This file is the one that
// answers "does it actually work over the wire", because every part of the claim
// that can be got wrong silently lives at the seam:
//
//   - the listener only REQUESTS a client certificate (tls.RequestClientCert),
//     so an absent one is the ordinary case and must refuse rather than panic;
//   - the fingerprint the gate computes from r.TLS.PeerCertificates[0] must be
//     byte-identical to the one an operator bound from the peer's certificate
//     file — a second construction is well-formed and NEVER matches, and
//     presents as a peering configuration fault rather than as a hashing bug;
//   - the production busTLSConfig, not a test-local tls.Config, has to be what
//     carries it.
//
// It stands the gate directly on the listener rather than on a mounted route:
// RELAY-20 owns the peer routes, and this task must not pre-empt which paths
// they are.

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	ipLocalBus  = "bus-live-local"
	ipRemoteBus = "bus-live-remote"
	ipThirdBus  = "bus-live-third"
	ipProbePath = "/relay45-live-probe"
)

// ipLateLog defers binding the *wal.Log until wal.Open has replayed into the
// store: the store must exist before the log that replays into it, and the log
// must exist before the store can write. Same indirection cmd/agent-bus's own
// invite wiring uses, and for the same reason.
type ipLateLog struct{ l *wal.Log }

func (d *ipLateLog) Write(e wal.Entry) (wal.Committed, error) { return d.l.Write(e) }

// ipBus is one bus's worth of live fixture: its certificate material, its data
// directory, and (for the local bus) its durable peer store.
type ipBus struct {
	dir      string
	material *buscert.Material
	store    *relay.PeerStore
}

// ipNewBus mints REAL bus key material in a fresh data directory. The
// certificate is the same one a running bus serves AND the same one it presents
// as a client when it dials a peer — which is exactly why the inbound and
// outbound tables must be looked up separately and never inverted into one map.
func ipNewBus(t *testing.T, busID string) *ipBus {
	t.Helper()
	dir := t.TempDir()
	m, err := buscert.LoadOrCreate(dir, buscert.Options{BusID: busID, Hosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatalf("buscert.LoadOrCreate(%s): %v", busID, err)
	}
	return &ipBus{dir: dir, material: m}
}

// ipOpenStore opens a durable peer store over a real *wal.Log in b.dir.
func (b *ipBus) ipOpenStore(t *testing.T, busID string) *relay.PeerStore {
	t.Helper()
	d := &ipLateLog{}
	st, err := relay.NewPeerStore(relay.PeerStoreOptions{BusID: busID, Dir: b.dir, Durable: d})
	if err != nil {
		t.Fatalf("relay.NewPeerStore: %v", err)
	}
	lg, err := wal.Open(wal.LogOptions{Dir: b.dir, Applier: st})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", b.dir, err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	d.l = lg
	b.store = st
	return st
}

// ipProbeResult is what the probe handler reports back about the connection: the
// principal the gate resolved, if any.
type ipProbeResult struct {
	PeerBus     string `json:"peer_bus"`
	Fingerprint string `json:"client_cert_sha256"`
}

// ipServe starts a REAL TLS listener using the PRODUCTION busTLSConfig, with the
// gate wrapped around a probe handler, and returns its address.
func ipServe(t *testing.T, local *ipBus, store *relay.PeerStore) string {
	t.Helper()

	srv := httpapi.New(httpapi.Options{
		Identity:       httpapi.StaticIdentity(ipLocalBus),
		Logger:         logging.New(io.Discard, logging.LevelError),
		PeerPrincipals: store,
	})

	handler := srv.RequirePeerPrincipal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := httpapi.PeerPrincipalFromContext(r.Context())
		if !ok {
			// Unreachable behind the gate. Answered as a 500 rather than
			// asserted, so a regression shows up as a failed assertion in the
			// test rather than as a panic in a server goroutine.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ipProbeResult{PeerBus: p.BusID, Fingerprint: p.CertFingerprint.String()})
	}))

	cfg, err := busTLSConfig(local.material)
	if err != nil {
		t.Fatalf("busTLSConfig: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hs := &http.Server{Handler: handler, TLSConfig: cfg, ReadHeaderTimeout: busTestTimeout}
	go func() { _ = hs.Serve(tls.NewListener(ln, cfg)) }()
	t.Cleanup(func() { _ = hs.Close() })
	return ln.Addr().String()
}

// ipDial issues a request to the live bus, optionally presenting clientCert as a
// TLS CLIENT CERTIFICATE. The bus's own certificate is the sole root — no
// InsecureSkipVerify, here or anywhere (invariant 11).
func ipDial(t *testing.T, addr string, local *ipBus, clientCert *tls.Certificate) (int, ipProbeResult) {
	t.Helper()

	pem := mustReadFile(t, filepath.Join(local.dir, buscert.CertFileName))
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("the local bus wrote no usable PEM certificate")
	}
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	tr := &http.Transport{TLSClientConfig: cfg}
	defer tr.CloseIdleConnections()

	c := &http.Client{Timeout: busTestTimeout, Transport: tr}
	resp, err := c.Post(busURL(addr, ipProbePath), "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", ipProbePath, err)
	}
	defer resp.Body.Close()

	var out ipProbeResult
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decoding the probe reply: %v", err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode, out
}

// TestInboundPeerPrincipalBinding is the live A <- B admission, and its
// refusals, over real TLS.
func TestInboundPeerPrincipalBinding(t *testing.T) {
	local := ipNewBus(t, ipLocalBus)
	remote := ipNewBus(t, ipRemoteBus)
	stranger := ipNewBus(t, ipThirdBus)

	store := local.ipOpenStore(t, ipLocalBus)

	// The operator binds the ADJACENT bus's certificate, read from that bus's
	// certificate file exactly as they would copy it out of band.
	remoteFP := buscert.FingerprintOf(remote.material.Certificate())
	if _, err := store.PutTrust(relay.BusTrust{
		BusID:                        ipRemoteBus,
		SigningKeys:                  []ed25519.PublicKey{remote.material.SigningPublicKey()},
		PeerClientTLSCertFingerprint: remoteFP,
	}); err != nil {
		t.Fatalf("binding the adjacent bus's client certificate: %v", err)
	}

	addr := ipServe(t, local, store)

	// --- A <- B: the bound peer is admitted, and named -----------------------
	remoteTLS := remote.material.TLSCertificate()
	status, got := ipDial(t, addr, local, &remoteTLS)
	if status != http.StatusOK {
		t.Fatalf("the bound peer got status %d, want %d; a configured peering was refused over a real handshake", status, http.StatusOK)
	}
	if got.PeerBus != ipRemoteBus {
		t.Fatalf("the live connection resolved to %q, want %q", got.PeerBus, ipRemoteBus)
	}
	if got.Fingerprint != remoteFP.String() {
		t.Fatalf("the gate reported fingerprint %s, want %s; the value computed from the live handshake must equal the one the operator bound", got.Fingerprint, remoteFP)
	}

	// --- NO CERTIFICATE: the ordinary case on this listener, and a refusal ---
	if status, _ := ipDial(t, addr, local, nil); status != http.StatusForbidden {
		t.Fatalf("a connection with NO client certificate got status %d, want %d", status, http.StatusForbidden)
	}

	// --- A WRONG certificate: a real bus, no binding -------------------------
	strangerTLS := stranger.material.TLSCertificate()
	if status, _ := ipDial(t, addr, local, &strangerTLS); status != http.StatusForbidden {
		t.Fatalf("an unbound bus's certificate got status %d, want %d", status, http.StatusForbidden)
	}

	// --- REVOCATION takes admission away, over the wire ----------------------
	if _, err := store.RemoveTrust(ipRemoteBus); err != nil {
		t.Fatalf("RemoveTrust: %v", err)
	}
	if status, _ := ipDial(t, addr, local, &remoteTLS); status != http.StatusForbidden {
		t.Fatalf("after revocation the peer got status %d, want %d; a revoked transport credential still admitted", status, http.StatusForbidden)
	}
}

// TestInboundPeerPrincipalRouteForIsolation is the live form of the regression
// this task exists for: `-route-for` legitimately puts ONE next-hop fingerprint
// on records for SEVERAL destination buses, and none of it may touch inbound
// identity.
func TestInboundPeerPrincipalRouteForIsolation(t *testing.T) {
	local := ipNewBus(t, ipLocalBus)
	remote := ipNewBus(t, ipRemoteBus)

	store := local.ipOpenStore(t, ipLocalBus)
	remoteFP := buscert.FingerprintOf(remote.material.Certificate())

	if _, err := store.PutTrust(relay.BusTrust{
		BusID:                        ipRemoteBus,
		SigningKeys:                  []ed25519.PublicKey{remote.material.SigningPublicKey()},
		PeerClientTLSCertFingerprint: remoteFP,
	}); err != nil {
		t.Fatalf("binding the adjacent bus's client certificate: %v", err)
	}

	// EXACTLY WHAT `peer add -bus-id busB -url https://b:8443 -tls-fingerprint
	// <fpB> -route-for busC` WRITES: busB's own route, and a route for busC
	// carrying busB's ADDRESS and busB's NEXT-HOP PIN. One fingerprint, two bus
	// ids — the ambiguity that makes a fingerprint -> bus id lookup forbidden
	// over route records.
	for _, dest := range []string{ipRemoteBus, ipThirdBus} {
		if _, err := store.Put(relay.PeerConfig{
			BusID:                     dest,
			BaseURL:                   "https://b.internal:8443",
			NextHopTLSCertFingerprint: remoteFP,
		}); err != nil {
			t.Fatalf("Put(route for %s): %v", dest, err)
		}
	}

	addr := ipServe(t, local, store)
	remoteTLS := remote.material.TLSCertificate()
	status, got := ipDial(t, addr, local, &remoteTLS)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.PeerBus != ipRemoteBus {
		t.Fatalf("an inbound %s connection resolved to %q; a -route-for record for a THIRD bus changed inbound identity, which is the exact spoof RELAY-45 exists to prevent", ipRemoteBus, got.PeerBus)
	}

	// Changing the next-hop pin on the third bus's route — or removing the
	// adjacent bus's route entirely — cannot move the answer.
	other := ipNewBus(t, ipThirdBus)
	if _, err := store.Put(relay.PeerConfig{
		BusID:                     ipThirdBus,
		BaseURL:                   "https://b.internal:8443",
		NextHopTLSCertFingerprint: buscert.FingerprintOf(other.material.Certificate()),
	}); err != nil {
		t.Fatalf("re-pinning the third bus's route: %v", err)
	}
	if _, err := store.Remove(ipRemoteBus); err != nil {
		t.Fatalf("Remove(the adjacent bus's route): %v", err)
	}
	status, got = ipDial(t, addr, local, &remoteTLS)
	if status != http.StatusOK || got.PeerBus != ipRemoteBus {
		t.Fatalf("after the route table changed underneath it the live connection resolved to (%q, status %d), want (%q, %d)", got.PeerBus, status, ipRemoteBus, http.StatusOK)
	}

	// And the third bus's OWN certificate — which is now a next-hop pin on a
	// route record — still names nobody inbound.
	otherTLS := other.material.TLSCertificate()
	if status, _ := ipDial(t, addr, local, &otherTLS); status != http.StatusForbidden {
		t.Fatalf("a certificate that appears ONLY as an outbound next-hop pin was admitted with status %d, want %d", status, http.StatusForbidden)
	}
}
