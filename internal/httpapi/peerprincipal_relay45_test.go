package httpapi

// RELAY-45 at the HTTP plane: the gate that turns a presented client
// certificate into an adjacent-bus principal, or into a refusal.
//
// These are white-box tests because the thing under test attaches a value to a
// request context and there is no peer route yet to observe it from — RELAY-20
// mounts those. A recording handler behind RequirePeerPrincipal is the only way
// to prove the principal REACHES a handler rather than being computed, logged
// and dropped, and the only way to prove the agent principal does NOT reach it.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/logging"
)

const (
	ppLocalBus  = "bus-http-local"
	ppRemoteBus = "bus-http-remote"
	ppOtherBus  = "bus-http-other"
	// ppProbePath is registered nowhere, so a request that reaches the recording
	// handler got there through the wrapper and nothing else.
	ppProbePath = "/relay45-gate-probe"
)

// ppResolver is a stub InboundPeerPrincipals: a fixed fingerprint -> bus map,
// with everything else refused. It is deliberately NOT a *relay.PeerStore — this
// package's job is the gate, and the durable binding is proved in internal/relay
// against real records.
type ppResolver struct {
	bound map[buscert.Fingerprint]string
	calls int
}

var errPPUnbound = errors.New("stub: no adjacent bus principal is bound to that certificate")

func (r *ppResolver) InboundPeerPrincipal(cert *x509.Certificate) (string, error) {
	r.calls++
	if cert == nil {
		return "", errPPUnbound
	}
	if busID, ok := r.bound[buscert.FingerprintOf(cert)]; ok {
		return busID, nil
	}
	return "", errPPUnbound
}

// ppCertFor mints a REAL self-signed bus certificate, so the fingerprint the
// gate computes from r.TLS.PeerCertificates[0] is computed from DER that really
// arrived over a handshake shape, not from a hand-written digest.
func ppCertFor(t *testing.T, busID string) *x509.Certificate {
	t.Helper()
	m, err := buscert.LoadOrCreate(t.TempDir(), buscert.Options{BusID: busID})
	if err != nil {
		t.Fatalf("buscert.LoadOrCreate(%s): %v", busID, err)
	}
	return m.Certificate()
}

// ppServer builds a *Server with the given resolver plus the buffer capturing
// its log at debug level.
func ppServer(t *testing.T, res InboundPeerPrincipals) (*Server, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	srv := New(Options{
		Identity:       StaticIdentity(ppLocalBus),
		Logger:         logging.New(&logBuf, logging.LevelDebug),
		PeerPrincipals: res,
	})
	return srv, &logBuf
}

// ppRequest builds a request carrying certs as the peer chain, or no TLS state
// at all when certs is nil.
func ppRequest(certs []*x509.Certificate) *http.Request {
	req := httptest.NewRequest(http.MethodPost, ppProbePath, strings.NewReader("{}"))
	if certs != nil {
		req.TLS = &tls.ConnectionState{PeerCertificates: certs}
	}
	return req
}

// TestInboundPeerPrincipalBinding proves the admitted path end to end at this
// layer: a bound certificate yields the peer principal, the principal REACHES
// the handler, it names a BUS and not an agent, and the agent principal that may
// have been attached upstream is NOT visible to a peer handler.
func TestInboundPeerPrincipalBinding(t *testing.T) {
	cert := ppCertFor(t, ppRemoteBus)
	res := &ppResolver{bound: map[buscert.Fingerprint]string{buscert.FingerprintOf(cert): ppRemoteBus}}
	srv, logBuf := ppServer(t, res)

	var (
		seen      PeerPrincipal
		seenOK    bool
		seenAgent string
		agentOK   bool
		ran       bool
	)
	h := srv.RequirePeerPrincipal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		seen, seenOK = PeerPrincipalFromContext(r.Context())
		_, agentOK = PrincipalFromContext(r.Context())
		seenAgent = AgentIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	// The request carries a VALID agent session principal as well, which is the
	// realistic case: a peer bus is also an enrolled principal. It must not be
	// the thing that authorises the peer route, and it must not be readable by
	// the peer handler at all.
	req := ppRequest([]*x509.Certificate{cert})
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyPrincipal,
		auth.Principal{AgentID: ppLocalBus + ".agent-imposter"}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !ran {
		t.Fatalf("the handler did not run; the gate refused a bound certificate (status %d, body %q)", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if !seenOK {
		t.Fatalf("the handler saw no peer principal; it was computed and dropped")
	}
	if seen.BusID != ppRemoteBus {
		t.Fatalf("peer principal bus id = %q, want %q", seen.BusID, ppRemoteBus)
	}
	// INVARIANT 2: a peer principal names a BUS. A '.' would mean an agent id
	// had been handed back as a bus principal.
	if strings.Contains(seen.BusID, ".") {
		t.Fatalf("peer principal = %q; it must be a BARE bus id, never a qualified <bus-id>.<agent-id>", seen.BusID)
	}
	if seen.CertFingerprint != buscert.FingerprintOf(cert) {
		t.Fatalf("peer principal fingerprint = %s, want %s; there must be exactly ONE construction", seen.CertFingerprint, buscert.FingerprintOf(cert))
	}
	if got := PeerBusIDFromContext(req.Context()); got != "" {
		t.Fatalf("PeerBusIDFromContext on the ORIGINAL request = %q, want \"\"; the principal must only exist on the request handed downstream", got)
	}

	// THE AGENT CREDENTIAL IS NOT A PEER CREDENTIAL, and here it is not even
	// visible: a peer route carries exactly one principal, the transport one.
	if agentOK || seenAgent != "" {
		t.Fatalf("a peer handler saw agent principal %q (ok=%v); a session credential must never be readable as the authorisation subject of a peer route", seenAgent, agentOK)
	}

	if logged := logBuf.String(); !strings.Contains(logged, ppRemoteBus) {
		t.Fatalf("the admission was not logged with the peer bus id:\n%s", logged)
	}
}

// TestInboundPeerPrincipalRejectsWrongAndUnboundCert is the refusal surface.
// EVERY case must produce the same status, the same body and NO handler
// execution — the client learns only that this connection is not an authorised
// peer, never which of the six reasons applied.
func TestInboundPeerPrincipalRejectsWrongAndUnboundCert(t *testing.T) {
	bound := ppCertFor(t, ppRemoteBus)
	stranger := ppCertFor(t, ppOtherBus)
	res := &ppResolver{bound: map[buscert.Fingerprint]string{buscert.FingerprintOf(bound): ppRemoteBus}}

	for _, tc := range []struct {
		name     string
		resolver InboundPeerPrincipals
		certs    []*x509.Certificate
	}{
		{
			// A route mounted behind the gate on a server that was never given
			// a resolver. There is NO permissive default.
			name:     "no resolver configured",
			resolver: nil,
			certs:    []*x509.Certificate{bound},
		},
		{
			name:     "not over TLS at all",
			resolver: res,
			certs:    nil,
		},
		{
			// The MAJORITY case on this listener: tls.RequestClientCert means
			// most connections present nothing. It must refuse, not panic.
			name:     "TLS with no client certificate",
			resolver: res,
			certs:    []*x509.Certificate{},
		},
		{
			name:     "a certificate no binding names",
			resolver: res,
			certs:    []*x509.Certificate{stranger},
		},
		{
			// THE CHAIN SPOOF. The leaf is the attacker's; the victim's
			// (entirely public) certificate is appended at index 1. A gate that
			// SEARCHED the chain would admit this as the victim, although the
			// handshake proved possession of the LEAF's private key alone.
			name:     "an unbound leaf with a BOUND certificate appended to the chain",
			resolver: res,
			certs:    []*x509.Certificate{stranger, bound},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, logBuf := ppServer(t, tc.resolver)
			ran := false
			h := srv.RequirePeerPrincipal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ran = true
				w.WriteHeader(http.StatusNoContent)
			}))

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, ppRequest(tc.certs))

			if ran {
				t.Fatalf("THE HANDLER RAN. A refused peer request must never reach the route handler")
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
			// NO CHALLENGE. A WWW-Authenticate: Bearer here would invite a
			// refused peer to retry with a session token — advertising exactly
			// the credential confusion this gate exists to prevent.
			if got := rec.Header().Get("WWW-Authenticate"); got != "" {
				t.Fatalf("WWW-Authenticate = %q, want none", got)
			}
			var body ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding %q: %v", rec.Body.String(), err)
			}
			// ONE reason for every refusal: distinguishing them would let anyone
			// holding a certificate enumerate which buses this bus has peered
			// with, and whether a binding had been revoked.
			if body.Error != peerPrincipalRefusal {
				t.Fatalf("error = %q, want the single fixed reason %q", body.Error, peerPrincipalRefusal)
			}
			// The LOG, by contrast, must say enough for an operator to act.
			if logBuf.Len() == 0 {
				t.Fatalf("the refusal was silent; an operator cannot configure a peering they are never told was refused")
			}
		})
	}

	// The chain case deserves its own explicit assertion about WHAT was asked:
	// the resolver must have been handed the LEAF, never the appended entry.
	probe := &ppCapturingResolver{inner: res}
	srv, _ := ppServer(t, probe)
	h := srv.RequirePeerPrincipal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), ppRequest([]*x509.Certificate{stranger, bound}))
	if probe.lastFingerprint != buscert.FingerprintOf(stranger) {
		t.Fatalf("the gate asked about %s, want the LEAF %s; it must index [0] and never iterate the chain",
			probe.lastFingerprint, buscert.FingerprintOf(stranger))
	}
	if probe.calls != 1 {
		t.Fatalf("the gate made %d resolver calls for one request, want exactly 1; more than one means it is walking the chain", probe.calls)
	}
}

// ppCapturingResolver records exactly which certificate the gate asked about.
type ppCapturingResolver struct {
	inner           InboundPeerPrincipals
	calls           int
	lastFingerprint buscert.Fingerprint
}

func (r *ppCapturingResolver) InboundPeerPrincipal(cert *x509.Certificate) (string, error) {
	r.calls++
	if cert != nil {
		r.lastFingerprint = buscert.FingerprintOf(cert)
	}
	return r.inner.InboundPeerPrincipal(cert)
}
