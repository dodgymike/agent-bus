package httpapi

// RELAY-20: the federation ingress mount.
//
// Three properties are under test, and they are the three the retired guard
// TestRelayPeerRoutesAreNotMountedYet used to assert syntactically and can no
// longer, now that the routes are actually served:
//
//  1. REGISTRATION IS ALL-OR-NOTHING. Miss any link of the chain — registry,
//     trust, any one handler, or the inbound principal resolver — and the peer
//     paths are not registered at all, so they are answered by default-deny and
//     the catch-all behind it, exactly like any other path this build does not
//     serve (401 anonymous, 404 once authenticated). Never a registered 503 and
//     never a registered 403: either is a claim that the surface is there.
//  2. EVERY REGISTERED PEER ROUTE IS GATED. No client certificate, an unbound
//     certificate, an EXPIRED certificate and a perfectly valid AGENT BEARER
//     TOKEN all get the same 403, and the relay handler behind the gate never
//     runs.
//  3. A BOUND, IN-DATE CERTIFICATE REACHES THE HANDLER, carrying the adjacent
//     bus principal and NO agent principal.
//
// These are white-box tests because the mount is unexported and because the
// interesting negatives are about which handler did NOT run.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	pmLocalBus  = "bus-mount-local"
	pmRemoteBus = "bus-mount-remote"
	pmIdemKey   = "relay20-mount-probe"
)

// pmTrust is a CrossBusTrust that pins nothing. It is enough for this file: the
// mount's job is to decide WHETHER a route exists and WHO may reach it, never
// what the relay ingress does with a verified message. Refusing every key is
// also the honest stub — an implementation that returned one would be asserting
// a peering this test never established.
type pmTrust struct{}

func (pmTrust) PinnedBusSigningKeys(string) ([]ed25519.PublicKey, error) {
	return nil, errors.New("stub: no bus signing key is pinned in this test")
}

func (pmTrust) AttestedSignerKey(string, attest.Attestation, []ed25519.PublicKey) (ed25519.PublicKey, error) {
	return nil, errors.New("stub: this test pins no origin bus, so no agent messaging key is attested")
}

// pmSurface builds a complete PeerSurface, recording every handshake that
// reaches the enrol handler together with the peer principal on its context.
//
// The enrol handler is the one used for the reachability assertions because its
// callback receives the request context, which is the only way to prove the
// principal travelled all the way THROUGH the mount rather than being computed
// and dropped at the gate.
type pmReached struct {
	calls     int
	busID     string
	peerBusID string
	sawPeer   bool
	sawAgent  bool

	// ackCalls and ackPeerBusID record what the ACK route was told the
	// AUTHENTICATED peer bus was (ACK-3). It is a separate pair from peerBusID
	// above because the ACK route reaches its principal by a different path — a
	// Go parameter supplied by servePeerAck, rather than a context lookup inside
	// the handler — and the two must not be able to cover for each other.
	ackCalls     int
	ackPeerBusID string
}

func pmSurface(t *testing.T, reached *pmReached) *PeerSurface {
	t.Helper()

	registry, err := relay.NewRegistry(relay.RegistryOptions{BusID: pmLocalBus})
	if err != nil {
		t.Fatalf("relay.NewRegistry: %v", err)
	}
	trust := pmTrust{}

	enroll, err := relay.NewHandler(relay.Config{
		BusID:       pmLocalBus,
		LocalRoster: func() []string { return nil },
		AcceptPeer: func(ctx context.Context, p relay.PeerRoster) error {
			reached.calls++
			reached.busID = p.BusID
			principal, ok := PeerPrincipalFromContext(ctx)
			reached.sawPeer = ok
			reached.peerBusID = principal.BusID
			_, reached.sawAgent = PrincipalFromContext(ctx)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("relay.NewHandler: %v", err)
	}

	relayIngest, err := relay.NewRelayHandler(relay.RelayConfig{
		BusID: pmLocalBus,
		AcceptRelay: func(context.Context, relay.RelayedMessage) (relay.RelayAcceptance, error) {
			return relay.RelayAcceptance{}, nil
		},
		Trust: trust,
	})
	if err != nil {
		t.Fatalf("relay.NewRelayHandler: %v", err)
	}

	roster, err := relay.NewRosterHandler(relay.RosterConfig{
		BusID: pmLocalBus,
		Apply: func(context.Context, relay.RosterUpdate, string, idem.Fingerprint) error { return nil },
	})
	if err != nil {
		t.Fatalf("relay.NewRosterHandler: %v", err)
	}

	// ACK-3. The obligation table is a REAL *relay.Outbox over a null durable
	// log, holding ONE obligation: this bus owes pmRemoteBus a copy of
	// pmAckCorrelationKey and owes NOBODY ELSE ANYTHING. That single asymmetry is
	// what makes TestPeerAckBindsToTheCertificateResolvedBus able to tell the
	// authenticated peer apart from any other name a request might carry.
	outbox, err := relay.NewOutbox(relay.OutboxOptions{BusID: pmLocalBus, Durable: pmNullDurable{}})
	if err != nil {
		t.Fatalf("relay.NewOutbox: %v", err)
	}
	if _, err := outbox.Enqueue(relay.OutboxJob{
		PeerBusID:       pmRemoteBus,
		OriginMessageID: pmAckCorrelationKey,
		Size:            5,
		ContentSHA256:   strings.Repeat("ab", 32),
	}); err != nil {
		t.Fatalf("outbox.Enqueue: %v", err)
	}
	ackIngest, err := relay.NewAckHandler(relay.AckConfig{
		BusID:       pmLocalBus,
		Obligations: outbox,
		Admit:       func(string) (func(), error) { return func() {}, nil },
		SettleAck: func(_ context.Context, s relay.SettledAck) (relay.AckSettlement, error) {
			reached.ackCalls++
			reached.ackPeerBusID = s.PeerBusID
			return relay.AckSettlement{}, nil
		},
	})
	if err != nil {
		t.Fatalf("relay.NewAckHandler: %v", err)
	}

	return &PeerSurface{
		Enroll:   enroll,
		Relay:    relayIngest,
		Roster:   roster,
		Ack:      ackIngest,
		Registry: registry,
		Trust:    trust,
	}
}

// pmCert mints a REAL self-signed bus certificate, so every fingerprint in this
// file is computed from DER that could have arrived over a handshake.
func pmCert(t *testing.T, busID string) *x509.Certificate {
	t.Helper()
	m, err := buscert.LoadOrCreate(t.TempDir(), buscert.Options{BusID: busID})
	if err != nil {
		t.Fatalf("buscert.LoadOrCreate(%s): %v", busID, err)
	}
	return m.Certificate()
}

// pmResolver binds exactly one fingerprint to one bus and refuses everything
// else, counting its calls so a test can prove the binding was never consulted
// on a path that should have refused earlier.
type pmResolver struct {
	bound map[buscert.Fingerprint]string
	calls int
}

func (r *pmResolver) InboundPeerPrincipal(cert *x509.Certificate) (string, error) {
	r.calls++
	if cert == nil {
		return "", errors.New("stub: no certificate")
	}
	if busID, ok := r.bound[buscert.FingerprintOf(cert)]; ok {
		return busID, nil
	}
	return "", errors.New("stub: no adjacent bus principal is bound to that certificate")
}

// pmServer builds a server with the given surface, resolver and clock.
func pmServer(t *testing.T, surface *PeerSurface, res InboundPeerPrincipals, now func() time.Time) (*Server, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	srv := New(Options{
		Identity:       StaticIdentity(pmLocalBus),
		Logger:         logging.New(&logBuf, logging.LevelDebug),
		PeerPrincipals: res,
		Peer:           surface,
		Now:            now,
	})
	return srv, &logBuf
}

// pmEnrolRequest is a WELL-FORMED peering handshake: if it is refused, it was
// refused by the gate and not by the payload validator.
func pmEnrolRequest(certs []*x509.Certificate) *http.Request {
	return pmEnrolRequestAt(relay.PeerEnrollPath, certs)
}

// pmPeerPaths is the surface under test, spelled through the constants so a
// path rename cannot leave this file asserting on a route nobody serves.
var pmPeerPaths = []string{relay.PeerEnrollPath, relay.PeerRelayPath, relay.PeerRosterPath, relay.PeerAckPath}

// TestPeerRoutesRegisterOnlyWithRegistryAndTrust is this task's proof command.
//
// The table is every way the chain can be incomplete, and the assertion in each
// incomplete case is the SAME and is deliberately strict: the path must not
// appear in Routes(), must not appear in PeerRoutes(), and must be answered by
// default-deny rather than by a registered handler. A 503 or a 403 from an
// unregistered surface would be a claim, readable by anyone, that this bus
// federates — which is the whole reason the choice is "not registered" rather
// than "registered and refusing".
//
// NOTE the residual this pins the honest version of: on a build that DOES
// federate an anonymous probe gets 403 rather than 401, so whether federation is
// served is observable pre-auth. That is recorded, with what bounds it and why
// the cheap fixes are worse, in peermount.go under KNOWN RESIDUAL 1.
func TestPeerRoutesRegisterOnlyWithRegistryAndTrust(t *testing.T) {
	full := func(t *testing.T) *PeerSurface { return pmSurface(t, &pmReached{}) }

	cases := []struct {
		name     string
		surface  func(t *testing.T) *PeerSurface
		resolver func() InboundPeerPrincipals
		want     bool // want the peer routes registered
	}{
		{
			name:     "the whole chain is present",
			surface:  full,
			resolver: func() InboundPeerPrincipals { return &pmResolver{} },
			want:     true,
		},
		{
			name:     "no peer surface at all: the ordinary non-federating build",
			surface:  func(*testing.T) *PeerSurface { return nil },
			resolver: func() InboundPeerPrincipals { return &pmResolver{} },
			want:     false,
		},
		{
			name: "no registry",
			surface: func(t *testing.T) *PeerSurface {
				s := full(t)
				s.Registry = nil
				return s
			},
			resolver: func() InboundPeerPrincipals { return &pmResolver{} },
			want:     false,
		},
		{
			name: "no trust",
			surface: func(t *testing.T) *PeerSurface {
				s := full(t)
				s.Trust = nil
				return s
			},
			resolver: func() InboundPeerPrincipals { return &pmResolver{} },
			want:     false,
		},
		{
			name: "no enrol handler",
			surface: func(t *testing.T) *PeerSurface {
				s := full(t)
				s.Enroll = nil
				return s
			},
			resolver: func() InboundPeerPrincipals { return &pmResolver{} },
			want:     false,
		},
		{
			name: "no relay handler",
			surface: func(t *testing.T) *PeerSurface {
				s := full(t)
				s.Relay = nil
				return s
			},
			resolver: func() InboundPeerPrincipals { return &pmResolver{} },
			want:     false,
		},
		{
			name: "no roster handler",
			surface: func(t *testing.T) *PeerSurface {
				s := full(t)
				s.Roster = nil
				return s
			},
			resolver: func() InboundPeerPrincipals { return &pmResolver{} },
			want:     false,
		},
		{
			// ACK-3. A surface missing the acknowledgement ingest registers
			// NOTHING, including the three routes that ARE present — "every field
			// or none" — because a bus that accepts a peering it can carry
			// messages for but cannot acknowledge is exactly the half-working
			// federation PeerSurface's doc refuses.
			name: "no ack handler",
			surface: func(t *testing.T) *PeerSurface {
				s := full(t)
				s.Ack = nil
				return s
			},
			resolver: func() InboundPeerPrincipals { return &pmResolver{} },
			want:     false,
		},
		{
			// The link most likely to be forgotten, and the one whose absence
			// would otherwise LOOK like it worked: the routes would exist and
			// answer 403 to every peer, which is indistinguishable from an
			// unbound certificate.
			name:     "a complete surface but no inbound principal resolver",
			surface:  full,
			resolver: func() InboundPeerPrincipals { return nil },
			want:     false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv, logBuf := pmServer(t, tc.surface(t), tc.resolver(), nil)

			registered := map[string]bool{}
			for _, p := range srv.Routes() {
				registered[p] = true
			}
			peerRoutes := map[string]bool{}
			for _, p := range srv.PeerRoutes() {
				peerRoutes[p] = true
			}

			for _, path := range pmPeerPaths {
				if registered[path] != tc.want {
					t.Errorf("Routes() contains %s = %v, want %v", path, registered[path], tc.want)
				}
				if peerRoutes[path] != tc.want {
					t.Errorf("PeerRoutes() contains %s = %v, want %v", path, peerRoutes[path], tc.want)
				}

				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, pmEnrolRequestAt(path, nil))

				if tc.want {
					// Registered: the gate answers, because no certificate was
					// presented. NOT 200, and NOT the 401 an unserved path gets.
					if rec.Code != http.StatusForbidden {
						t.Errorf("POST %s with no client certificate = %d, want 403 from the peer gate; body %s", path, rec.Code, rec.Body.String())
					}
					continue
				}
				// UNREGISTERED means the request is answered by default-deny
				// authMiddleware and the catch-all behind it, exactly like any
				// other path this build does not serve — 401 to an anonymous
				// caller, 404 once authenticated. What must NOT happen is a
				// registered 503 or a registered 403: either is a claim that the
				// surface is there.
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("POST %s = %d, want 401 (default-deny, indistinguishable from any unserved path).\n"+
						"An incomplete federation chain must leave these paths UNREGISTERED. A registered 503 or a "+
						"registered 403 tells a caller that this bus federates, which is the disclosure this choice "+
						"exists to avoid.\nbody: %s", path, rec.Code, rec.Body.String())
				}
				if rec.Code == http.StatusServiceUnavailable || rec.Code == http.StatusForbidden {
					t.Errorf("POST %s = %d; an unregistered peer surface must never answer from a registered handler", path, rec.Code)
				}
			}

			// A mis-wired build must be diagnosable from the inside, since it is
			// deliberately indistinguishable from the outside.
			if !tc.want && tc.surface(t) != nil {
				if !strings.Contains(logBuf.String(), "FEDERATION IS NOT SERVED") {
					t.Errorf("an incomplete PeerSurface was supplied and nothing said so in the log; "+
						"a 404 that is meant to be indistinguishable externally MUST be loud internally.\nlog: %s", logBuf.String())
				}
			}
		})
	}
}

// pmEnrolRequestAt is pmEnrolRequest for an arbitrary peer path. The body is
// only well-formed for the enrol path; every assertion using it on the other two
// is about a refusal that happens BEFORE any body is read.
func pmEnrolRequestAt(path string, certs []*x509.Certificate) *http.Request {
	body, _ := json.Marshal(relay.PeerEnrollRequest{BusID: pmRemoteBus, Agents: []string{pmRemoteBus + ".alpha-1"}})
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idem.HeaderName, pmIdemKey)
	if certs != nil {
		req.TLS = &tls.ConnectionState{PeerCertificates: certs}
	}
	return req
}

// TestMountedPeerRoutesAreGatedByTheCertificateAndNothingElse is the security
// half: the four ways a caller can arrive without a bound, in-date certificate
// all get the same 403, and the relay handler behind the gate never runs.
//
// The last case is the one that matters most and the one a syntactic guard could
// never see: a VALID AGENT BEARER TOKEN is not a peer credential. If the mount
// had been wired by adding the paths to unauthenticatedRoutes, or by trusting
// authMiddleware to be the gate, that case would return 200.
func TestMountedPeerRoutesAreGatedByTheCertificateAndNothingElse(t *testing.T) {
	bound := pmCert(t, pmRemoteBus)
	stranger := pmCert(t, "bus-mount-stranger")
	res := &pmResolver{bound: map[buscert.Fingerprint]string{buscert.FingerprintOf(bound): pmRemoteBus}}

	var reached pmReached
	srv, _ := pmServer(t, pmSurface(t, &reached), res, nil)

	cases := []struct {
		name  string
		certs []*x509.Certificate
		token string
	}{
		{name: "no TLS at all", certs: nil},
		{name: "TLS but no client certificate presented", certs: []*x509.Certificate{}},
		{name: "a certificate bound to nobody", certs: []*x509.Certificate{stranger}},
		{
			// THE CHAIN IS NOT SEARCHED. The peer controls every entry it sends;
			// the handshake proves possession of the LEAF's key alone, so a
			// stranger's leaf with the bound certificate appended must refuse.
			name:  "an unbound leaf with the bound certificate appended at index 1",
			certs: []*x509.Certificate{stranger, bound},
		},
		{
			// The whole point. A bearer token authenticates an AGENT; it can
			// never authenticate a BUS.
			name:  "a bearer token and no client certificate",
			certs: []*x509.Certificate{},
			token: "aValidLookingBearerTokenValueThatIsNotACertificate",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range pmPeerPaths {
				reached.calls = 0
				req := pmEnrolRequestAt(path, tc.certs)
				if tc.token != "" {
					req.Header.Set("Authorization", "Bearer "+tc.token)
				}

				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)

				if rec.Code != http.StatusForbidden {
					t.Errorf("POST %s (%s) = %d, want 403", path, tc.name, rec.Code)
				}
				if reached.calls != 0 {
					t.Errorf("POST %s (%s) reached the federation handler %d time(s); a refusal must be terminal, not advisory",
						path, tc.name, reached.calls)
				}
				// One fixed reason for every failure: distinguishing them would
				// let anyone with a certificate probe which buses this bus has
				// peered with.
				var body ErrorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("refusal body is not the standard JSON envelope: %v (%s)", err, rec.Body.String())
				}
				if body.Error != peerPrincipalRefusal {
					t.Errorf("refusal reason for %q = %q, want the one fixed string %q; a per-case reason is an enumeration oracle",
						tc.name, body.Error, peerPrincipalRefusal)
				}
				if got := rec.Header().Get("WWW-Authenticate"); got != "" {
					t.Errorf("the peer refusal carried WWW-Authenticate: %q. A Bearer challenge invites a refused peer to "+
						"retry with a session token, which advertises exactly the credential confusion this gate prevents", got)
				}
			}
		})
	}
}

// pmFailClosedResolver is the adversarial resolver: it reports a bus id AND an
// error at the same time, which is what an ambiguous binding
// (relay.ErrAmbiguousInboundPeerCert) and a withdrawn one look like from here if
// an implementation ever became careless about its return values.
//
// The whole fingerprint-first design is sound ONLY because uniqueness is
// enforced at WRITE and ambiguity fails closed at READ. This stub inverts the
// second half so the gate's own behaviour can be pinned independently of
// internal/relay: an error must refuse, whatever else came back with it.
type pmFailClosedResolver struct {
	busID string
	err   error
	calls int
}

func (r *pmFailClosedResolver) InboundPeerPrincipal(*x509.Certificate) (string, error) {
	r.calls++
	return r.busID, r.err
}

// pmServerWithAuth is pmServer plus a REAL enrolment and session authority, so a
// test can obtain a genuine bearer token for this bus and present it on a peer
// route. Same bus id throughout, because the relay handlers validate against it.
func pmServerWithAuth(t *testing.T, surface *PeerSurface, res InboundPeerPrincipals) (*Server, *auth.Service) {
	t.Helper()
	minter, err := ids.NewAgentIDMinter(pmLocalBus, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	svc, err := auth.NewService(auth.Options{Minter: minter})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}
	var logBuf bytes.Buffer
	srv := New(Options{
		Identity:       StaticIdentity(pmLocalBus),
		Logger:         logging.New(&logBuf, logging.LevelDebug),
		Auth:           svc,
		PeerPrincipals: res,
		Peer:           surface,
	})
	return srv, svc
}

// TestAgentSessionTokenIsNeverReadAsAPeerCredential is the WRAPPER-ORDER test,
// and it is the one that had to be written from scratch rather than adapted.
//
// # WHAT IT PINS
//
// RequirePeerPrincipal ends by SHADOWING the agent principal out of the context:
//
//	ctx = context.WithValue(ctx, ctxKeyPrincipal, noAgentPrincipal{})
//
// That shadow holds only while this wrapper is the INNERMOST auth-bearing one.
// Nested the other way — RequirePeerPrincipal(authMiddleware(h)) — authMiddleware
// runs INSIDE it and re-attaches the agent principal OVER the shadow, so a peer
// handler can read a session-derived AGENT identity and act on it as though it
// had authorised the peer request. That is a session credential accepted as a
// peer-bus credential: exactly the confusion invariant 11's cross-check exists
// to prevent, and exactly what invariant 3 forbids blurring.
//
// IT FAILS SILENTLY. Every positive test still passes in the wrong order,
// because a legitimate peer's request succeeds either way. Only a request
// carrying BOTH a bound client certificate AND a valid agent session token can
// see it.
//
// # WHY THE EARLIER VERSION OF THIS ASSERTION WAS WORTHLESS
//
// The security gate proved that the `sawAgent` check in
// TestMountedPeerRouteAdmitsABoundCertificateAndCarriesThePrincipal is
// STRUCTURALLY INCAPABLE OF FAILING: that server is built with no Auth service,
// so no agent principal exists to shadow and the assertion passes whatever the
// nesting is. This test supplies a real auth service, runs the real
// enrol -> begin -> sign -> complete handshake, and presents the resulting token
// — so the value being asserted absent is one that genuinely exists.
//
// # AND IT RECORDS A SECOND FACT ABOUT THIS MOUNT
//
// Under RELAY-20's wiring, authMiddleware SKIPS a peer route entirely
// (s.isPeerRoute), so it never attaches an agent principal there in the first
// place. The shadow is therefore DEFENCE IN DEPTH here rather than the only
// thing standing between the two credentials. Both are asserted, because the
// mount could later change to run authMiddleware over peer routes and the shadow
// would become load-bearing again without anybody noticing.
//
// MUTATION EVIDENCE, so "this test can fail" is a measurement rather than a
// hope. Three mutations were run against it:
//
//	RequirePeerPrincipal(authMiddleware(h))   still GREEN — the inner
//	                                          authMiddleware hits its own
//	                                          isPeerRoute branch and attaches
//	                                          nothing. The nesting is absorbed.
//	remove the isPeerRoute skip               still GREEN — the principal IS now
//	                                          attached, and the shadow removes
//	                                          it. This is the shadow working.
//	remove the skip AND the shadow            RED, with the message below and the
//	                                          agent id "…​.peerprobe-1" printed.
//
// So the two mechanisms are independent, either alone suffices, and this test
// observes both. It is NOT a substitute for keeping the nesting right — the
// first line shows the nesting is currently masked by the skip, which is exactly
// the kind of thing that stops being true after a refactor.
func TestAgentSessionTokenIsNeverReadAsAPeerCredential(t *testing.T) {
	bound := pmCert(t, pmRemoteBus)
	res := &pmResolver{bound: map[buscert.Fingerprint]string{buscert.FingerprintOf(bound): pmRemoteBus}}
	var reached pmReached
	srv, svc := pmServerWithAuth(t, pmSurface(t, &reached), res)

	token, agentID := mwHandshake(t, srv, "peerprobe")

	// THE CONTROL, without which the whole test could pass on an inert token.
	// This is the value the handler must NOT see; proving it is real is what
	// makes its absence downstream meaningful.
	principal, err := svc.Authenticate(token)
	if err != nil {
		t.Fatalf("the token from the handshake does not authenticate (%v); the absence assertion below would then be "+
			"proving nothing at all", err)
	}
	if principal.AgentID != agentID {
		t.Fatalf("Authenticate(token).AgentID = %q, want %q", principal.AgentID, agentID)
	}

	req := pmEnrolRequest([]*x509.Certificate{bound})
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s with a bound certificate AND a valid session token = %d, want 200; body %s",
			relay.PeerEnrollPath, rec.Code, rec.Body.String())
	}
	if reached.calls != 1 {
		t.Fatalf("the federation handler ran %d time(s), want 1", reached.calls)
	}
	if !reached.sawPeer || reached.peerBusID != pmRemoteBus {
		t.Errorf("peer principal on the handler = (%v, %q), want (true, %q)", reached.sawPeer, reached.peerBusID, pmRemoteBus)
	}

	// THE ASSERTION. A valid, live agent principal for THIS request existed and
	// the peer handler must not have seen it.
	if reached.sawAgent {
		t.Errorf("the federation handler saw an AGENT principal on a request carrying a valid session token.\n"+
			"That is a session credential being readable as a peer-bus credential. Check the wrapper nesting: "+
			"authMiddleware(RequirePeerPrincipal(h)) is CORRECT; RequirePeerPrincipal(authMiddleware(h)) re-attaches "+
			"the agent principal OVER the shadow and every other test still passes.\n"+
			"The token authenticated as %q.", agentID)
	}
}

// TestPeerRoutesSetHasExactlyOneWriter is the AST guard on the field the whole
// bearer-skip rests on (reviewer gate, RELAY-20).
//
// authMiddleware skips the bearer requirement for any path in s.peerRoutes. That
// is only safe because mountPeerRoute is the SOLE writer and wraps the handler
// in RequirePeerPrincipal in the same function. Nothing structural enforced
// "sole writer" — it was a claim in a comment — and this package has a dozen
// non-test files, any of which could write to the map without the compiler
// objecting.
//
// A BEHAVIOURAL TEST CANNOT COVER THIS. TestEveryRecordedPeerRouteIsActuallyGated
// walks the set a server ACTUALLY built, so it catches a bad write only on a
// code path some test happens to exercise. A file that adds a path under a
// configuration no test builds is invisible to it and served to anyone.
//
// It is an AST walk rather than a grep, for client/guard_test.go's reason: the
// several comments in this package that discuss s.peerRoutes are correct work,
// and a guard that fails correct work is a guard someone deletes.
func TestPeerRoutesSetHasExactlyOneWriter(t *testing.T) {
	const writerFile = "peermount.go"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading this package's directory: %v", err)
	}

	fset := token.NewFileSet()
	// mentions maps file -> every position naming the field.
	writes := map[string][]string{}
	var scanned int

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("reading %s: %v", name, rerr)
		}
		file, perr := parser.ParseFile(fset, name, src, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		scanned++

		// ANY MENTION OF THE FIELD, not only an assignment to it.
		//
		// A first draft matched assignment targets — `x.peerRoutes = …` and
		// `x.peerRoutes[k] = …` — and the security gate demonstrated the evasion
		// in one line:
		//
		//	m := s.peerRoutes
		//	m[pattern] = struct{}{}   // records the bearer-skip, gates nothing
		//
		// The alias is an ordinary read on the right-hand side, so no assignment
		// target names the field and the guard stayed green while two peer routes
		// were recorded WITHOUT RequirePeerPrincipal. A map is a reference: there
		// is no such thing as a harmless read of it.
		//
		// Matching every selector is therefore the rule, and it costs nothing —
		// every use in this package today is in peermount.go. Comments are
		// unaffected: this is an AST walk, and authmw.go's paragraph explaining
		// s.peerRoutes is correct work that a grep would have failed.
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "peerRoutes" {
				return true
			}
			writes[name] = append(writes[name], fset.Position(sel.Pos()).String())
			return true
		})
	}

	if scanned == 0 {
		t.Fatal("parsed 0 non-test .go files in this package; this guard inspected nothing, which is not a pass")
	}
	if len(writes[writerFile]) == 0 {
		t.Fatalf("no mention of s.peerRoutes was found in %s.\n"+
			"Either the mount moved — update writerFile in this test, as a reviewed line — or nothing populates the set, "+
			"in which case authMiddleware demands a bearer token on every peer route and federation is silently dead.",
			writerFile)
	}
	for file, sites := range writes {
		if file == writerFile {
			continue
		}
		t.Errorf("%s names s.peerRoutes at %v.\n"+
			"Membership of that set is what makes authMiddleware SKIP the bearer requirement for a path. It is safe only "+
			"because mountPeerRoute records a path in the same function that wraps its handler in RequirePeerPrincipal — "+
			"a write anywhere else can record a path WITHOUT gating it, which serves that path to anyone with no "+
			"credential of any kind, while every positive test still passes.\n"+
			"READING it is refused too, not only assigning to it: a map is a reference, so `m := s.peerRoutes` followed "+
			"by `m[p] = struct{}{}` is a write that no assignment target names. If you need the set, call PeerRoutes(), "+
			"which returns a copy.",
			file, sites)
	}
}

// TestPeerRouteMatchingAgreesWithTheMuxOnEverySpelling pins the property the
// bearer-skip rests on, over a corpus of hostile spellings.
//
// isPeerRoute and http.ServeMux must agree about which requests reach a peer
// handler. TODAY they agree BY CONSTRUCTION — both key on r.URL.Path, and the
// mux exact-matches a pattern with no trailing slash — but that is a property of
// the Go version this module pins, NOT of anything written here. Go 1.22 rebuilt
// ServeMux around escaped-path and per-segment matching. So this is a
// version-sensitive assumption with nothing else holding it, which is exactly
// the kind that survives a toolchain bump unnoticed.
//
// ONE DIRECTION IS FAIL-CLOSED AND THE OTHER IS FAIL-OPEN, which is why the
// assertion is asymmetric:
//
//	isPeerRoute says no, mux serves a peer handler -> a bearer is demanded on a
//	                                                 peer route. Annoying; SAFE.
//	isPeerRoute says YES, mux serves something ELSE -> the bearer check is skipped
//	                                                 for a NON-peer handler.
//	                                                 THAT is the hole.
//
// The corpus is checked for the second: for every spelling, either the response
// is a refusal (401 or 403 — nothing was served), or it was served by a peer
// handler with the gate having run.
func TestPeerRouteMatchingAgreesWithTheMuxOnEverySpelling(t *testing.T) {
	var reached pmReached
	srv, _ := pmServer(t, pmSurface(t, &reached), &pmResolver{}, nil)

	spellings := []string{
		relay.PeerEnrollPath,
		"//v1/peer/enroll",
		"/v1/peer/enroll/",
		"/v1/peer/enroll/../enroll",
		"/v1/peer/../peer/enroll",
		"/v1/peer/enroll.",
		"/v1/peer/enroll%20",
		"/v1/peer/%65nroll",
		"/v1/peer%2Fenroll",
		"/v1/PEER/enroll",
		"/v1/Peer/Enroll",
		"/v1/peer/enroll;x=1",
		"/./v1/peer/enroll",
		"/v1/peer/relay/../enroll",
		"/v1/peer/",
		"/v1/peer",
	}

	for _, spelling := range spellings {
		spelling := spelling
		t.Run(spelling, func(t *testing.T) {
			reached.calls = 0
			req := httptest.NewRequest(http.MethodPost, spelling, strings.NewReader("{}"))
			// No credential of any kind, so anything that is SERVED without a
			// refusal has bypassed both authenticators.
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			switch rec.Code {
			case http.StatusUnauthorized, http.StatusForbidden:
				// Refused. Either authenticator answered; nothing was served.
			case http.StatusMovedPermanently, http.StatusPermanentRedirect:
				// The mux cleaned the path and redirected. Nothing was served,
				// and the redirect target goes through this same stack.
			default:
				t.Errorf("POST %q with NO credential = %d, and it was not a refusal or a redirect.\n"+
					"Either isPeerRoute matched a spelling the mux routes ELSEWHERE (so the bearer check was skipped "+
					"for a non-peer handler), or something was served unauthenticated. isPeerRoute and http.ServeMux "+
					"must agree on every spelling; today they do because both key on r.URL.Path and these patterns are "+
					"exact-match, which is a property of the pinned Go version and of nothing else.\nbody: %s",
					spelling, rec.Code, rec.Body.String())
			}
			if reached.calls != 0 {
				t.Errorf("POST %q reached the federation handler with no credential", spelling)
			}
		})
	}
}

// TestPeerGateFailsClosedOnAnyResolverError pins the read half of the
// fingerprint-first design at the HTTP plane.
//
// An error from InboundPeerPrincipal is a refusal, unconditionally — there is no
// "unknown, so allow", no partial answer honoured, and a bus id returned
// alongside an error is DISCARDED rather than used. Ambiguity in particular must
// never be resolved by picking one: that would be choosing which bus to
// impersonate.
func TestPeerGateFailsClosedOnAnyResolverError(t *testing.T) {
	cert := pmCert(t, pmRemoteBus)

	cases := []struct {
		name     string
		resolver *pmFailClosedResolver
	}{
		{name: "unknown or withdrawn binding", resolver: &pmFailClosedResolver{err: relay.ErrUnknownInboundPeerCert}},
		{name: "ambiguous binding", resolver: &pmFailClosedResolver{err: relay.ErrAmbiguousInboundPeerCert}},
		{
			// The nasty one: a bus id AND an error. If the gate read the id
			// first, an ambiguous certificate would authorise whichever bus the
			// store happened to name.
			name:     "a bus id returned alongside an error",
			resolver: &pmFailClosedResolver{busID: pmRemoteBus, err: relay.ErrAmbiguousInboundPeerCert},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var reached pmReached
			srv, _ := pmServer(t, pmSurface(t, &reached), tc.resolver, nil)

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, pmEnrolRequest([]*x509.Certificate{cert}))

			if rec.Code != http.StatusForbidden {
				t.Errorf("POST %s with %s = %d, want 403", relay.PeerEnrollPath, tc.name, rec.Code)
			}
			if reached.calls != 0 {
				t.Errorf("the federation handler ran %d time(s) for %s; an error from the binding is a refusal, "+
					"never a partial answer to be honoured", reached.calls, tc.name)
			}
			if tc.resolver.calls != 1 {
				t.Errorf("the binding was consulted %d time(s), want exactly 1; a second lookup would be a fallback, "+
					"and there is no fallback", tc.resolver.calls)
			}
		})
	}
}

// TestPeerGateNeverAssumesACertificateWasPresented is the coordinator's second
// cross-check, and it is about an assumption rather than a code path:
// tls.RequestClientCert REQUESTS a certificate and never requires one, so an
// absent client certificate is the ORDINARY case for every agent connection, not
// an exotic one.
//
// The assertion that carries it is the RESOLVER CALL COUNT. A gate that treated
// "we got here" as "a certificate was presented" would hand nil — or panic on
// index [0] — to the binding. Zero calls proves it neither reached for a
// certificate that was not there nor asked the binding about one.
func TestPeerGateNeverAssumesACertificateWasPresented(t *testing.T) {
	cases := []struct {
		name  string
		certs []*x509.Certificate
	}{
		{name: "no TLS state at all", certs: nil},
		{name: "TLS with an EMPTY peer chain, which is every agent connection", certs: []*x509.Certificate{}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			res := &pmResolver{}
			var reached pmReached
			srv, _ := pmServer(t, pmSurface(t, &reached), res, nil)

			for _, path := range pmPeerPaths {
				rec := httptest.NewRecorder()
				// A panic here would be recovered by net/http per connection in
				// production and would present as a mysteriously dropped
				// request; in the test it fails outright, which is the point.
				srv.ServeHTTP(rec, pmEnrolRequestAt(path, tc.certs))

				if rec.Code != http.StatusForbidden {
					t.Errorf("POST %s with %s = %d, want 403", path, tc.name, rec.Code)
				}
			}
			if res.calls != 0 {
				t.Errorf("the inbound binding was consulted %d time(s) with no certificate presented.\n"+
					"tls.RequestClientCert makes an absent certificate NORMAL; a gate that reaches for index [0] "+
					"regardless panics on almost every request.", res.calls)
			}
			if reached.calls != 0 {
				t.Errorf("the federation handler ran %d time(s) with no certificate presented", reached.calls)
			}
		})
	}
}

// TestMountedPeerRouteAdmitsABoundCertificateAndCarriesThePrincipal is the
// counterweight: without it, a mount that refused EVERYTHING would pass every
// assertion above.
func TestMountedPeerRouteAdmitsABoundCertificateAndCarriesThePrincipal(t *testing.T) {
	bound := pmCert(t, pmRemoteBus)
	res := &pmResolver{bound: map[buscert.Fingerprint]string{buscert.FingerprintOf(bound): pmRemoteBus}}

	var reached pmReached
	srv, _ := pmServer(t, pmSurface(t, &reached), res, nil)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, pmEnrolRequest([]*x509.Certificate{bound}))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s with a bound, in-date certificate = %d, want 200; body %s", relay.PeerEnrollPath, rec.Code, rec.Body.String())
	}
	if reached.calls != 1 {
		t.Fatalf("the federation handler ran %d time(s), want exactly 1", reached.calls)
	}
	if !reached.sawPeer {
		t.Error("the handler saw NO peer principal; the gate resolved one and it must reach the handler, not be computed and dropped")
	}
	if reached.peerBusID != pmRemoteBus {
		t.Errorf("peer principal bus id = %q, want %q", reached.peerBusID, pmRemoteBus)
	}
	if strings.Contains(reached.peerBusID, ".") {
		t.Errorf("peer principal bus id %q contains '.'; it names a BUS, never a fully-qualified <bus-id>.<agent-id> (invariant 2)", reached.peerBusID)
	}
	// The shadow. A peer handler must never be able to act on an agent identity.
	if reached.sawAgent {
		t.Error("an AGENT principal was visible to the federation handler; RequirePeerPrincipal must shadow it out, or a session " +
			"credential could be picked up as if it had authorised the peer request")
	}
}

// TestExpiredPeerCertificateIsRefusedBeforeTheBindingIsConsulted closes the gap
// DECISIONS.md recorded as harmless "only while nothing authorises on a client
// certificate" (task ca356fde). This mount is the first thing that does.
//
// The clock is moved rather than the certificate, so the fixture is a REAL bus
// certificate with a real validity window and the test drives the same code an
// aged certificate would.
//
// The resolver call count is the load-bearing assertion: it proves the validity
// check runs BEFORE the durable binding is consulted, so a binding can never
// outlive the credential it names.
func TestExpiredPeerCertificateIsRefusedBeforeTheBindingIsConsulted(t *testing.T) {
	bound := pmCert(t, pmRemoteBus)

	clocks := []struct {
		name string
		at   time.Time
	}{
		{name: "long after NotAfter", at: bound.NotAfter.Add(time.Hour)},
		{name: "one second after NotAfter", at: bound.NotAfter.Add(time.Second)},
		{name: "before NotBefore", at: bound.NotBefore.Add(-time.Hour)},
	}

	for _, c := range clocks {
		c := c
		t.Run(c.name, func(t *testing.T) {
			res := &pmResolver{bound: map[buscert.Fingerprint]string{buscert.FingerprintOf(bound): pmRemoteBus}}
			var reached pmReached
			srv, logBuf := pmServer(t, pmSurface(t, &reached), res, func() time.Time { return c.at })

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, pmEnrolRequest([]*x509.Certificate{bound}))

			if rec.Code != http.StatusForbidden {
				t.Errorf("POST %s with a certificate %s = %d, want 403", relay.PeerEnrollPath, c.name, rec.Code)
			}
			if reached.calls != 0 {
				t.Errorf("the federation handler ran %d time(s) for an out-of-date certificate", reached.calls)
			}
			if res.calls != 0 {
				t.Errorf("the inbound binding was consulted %d time(s) for an out-of-date certificate.\n"+
					"The validity window must be judged FIRST: otherwise an operator's binding outlives the credential it "+
					"names, and expiry — the only automatic bound on a leaked peer key — means nothing on this surface.", res.calls)
			}
			if !strings.Contains(logBuf.String(), "outside its own validity window") {
				t.Errorf("nothing in the log named the expiry; the client is told only the fixed refusal, so the log is the "+
					"ONLY place an operator can see why their federation stopped.\nlog: %s", logBuf.String())
			}
		})
	}

	// The counterweight, in the same test: the same certificate at a time INSIDE
	// its window is admitted. Without this, a check that refused every clock
	// would pass everything above.
	t.Run("inside the window it is admitted", func(t *testing.T) {
		res := &pmResolver{bound: map[buscert.Fingerprint]string{buscert.FingerprintOf(bound): pmRemoteBus}}
		var reached pmReached
		srv, _ := pmServer(t, pmSurface(t, &reached), res, func() time.Time { return bound.NotAfter.Add(-time.Hour) })

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, pmEnrolRequest([]*x509.Certificate{bound}))

		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s one hour before NotAfter = %d, want 200; body %s", relay.PeerEnrollPath, rec.Code, rec.Body.String())
		}
		if reached.calls != 1 {
			t.Fatalf("the federation handler ran %d time(s), want 1", reached.calls)
		}
	})
}

// TestCheckClientCertValidityRefusesWhatItCannotJudge pins the two inputs that
// must never be treated as a pass. Both are unreachable from a live handshake,
// which is exactly why they are tested: a validity check with a path that
// returns nil without having judged anything is how a silent accept gets in.
func TestCheckClientCertValidityRefusesWhatItCannotJudge(t *testing.T) {
	leaf := pmCert(t, pmRemoteBus)

	if err := checkClientCertValidity(nil, time.Now()); err == nil {
		t.Error("checkClientCertValidity(nil, now) = nil; there was no certificate to judge, so it must refuse")
	}
	if err := checkClientCertValidity(leaf, time.Time{}); err == nil {
		t.Error("checkClientCertValidity(leaf, zeroTime) = nil.\n" +
			"crypto/x509 substitutes time.Now() for a zero CurrentTime, so the verdict would be right by accident while " +
			"the log line beside it named the wrong instant. A caller with no clock has not judged anything.")
	}
	if err := checkClientCertValidity(leaf, leaf.NotAfter.Add(-time.Hour)); err != nil {
		t.Errorf("checkClientCertValidity(leaf, insideWindow) = %v, want nil", err)
	}
	err := checkClientCertValidity(leaf, leaf.NotAfter.Add(time.Hour))
	if err == nil {
		t.Fatal("an expired certificate was judged usable")
	}
	if !errors.Is(err, ErrClientCertNotYetValidOrExpired) {
		t.Errorf("expiry error = %v, want it to wrap ErrClientCertNotYetValidOrExpired so the log and a test can name the condition", err)
	}
	// The message must carry the whole window plus the instant judged: "expired"
	// alone cannot separate a stale peer certificate from a wrong local clock.
	for _, want := range []string{
		leaf.NotBefore.UTC().Format(time.RFC3339),
		leaf.NotAfter.UTC().Format(time.RFC3339),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expiry error %q does not name %q; an operator cannot tell a stale certificate from a skewed clock without the window", err, want)
		}
	}
}

// TestNoPeerPathIsOnTheUnauthenticatedAllowList is the standing prohibition,
// asserted mechanically rather than trusted to review.
//
// Adding a peer path to unauthenticatedRoutes would not document an existing
// exemption — it would CREATE an ungated federation ingress, and every positive
// test in this file would still pass, because a legitimate peer's request
// succeeds either way.
func TestNoPeerPathIsOnTheUnauthenticatedAllowList(t *testing.T) {
	for _, path := range pmPeerPaths {
		if IsUnauthenticatedRoute(path) {
			t.Errorf("%s is on unauthenticatedRoutes. A peer route is authenticated by the TLS client certificate, "+
				"never by nothing; putting it on the allow-list serves our roster, our routing table and our relay "+
				"ingest to an anonymous POST.", path)
		}
	}

	// And the mount refuses to register such a path even if someone adds it, so
	// the two can never be reconciled by accident. Driven through the real
	// helper with a path that IS on the allow-list.
	var reached pmReached
	srv, logBuf := pmServer(t, pmSurface(t, &reached), &pmResolver{}, nil)
	mux := http.NewServeMux()
	srv.mountPeerRoute(mux, RouteEnroll, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if srv.isPeerRoute(RouteEnroll) {
		t.Errorf("mountPeerRoute recorded %s as a peer route even though it is on the unauthenticated allow-list; "+
			"that combination is an unauthenticated path that authMiddleware would also skip", RouteEnroll)
	}
	if !strings.Contains(logBuf.String(), "REFUSING to mount a peer route") {
		t.Errorf("mountPeerRoute refused silently; a refusal that leaves a route unserved must say so.\nlog: %s", logBuf.String())
	}
}

// TestEveryRecordedPeerRouteIsActuallyGated is the invariant that makes
// authMiddleware's peer branch safe, checked over the REAL registered set rather
// than over a list written here.
//
// s.peerRoutes is what makes authMiddleware skip the bearer requirement. If a
// path could get into that set without RequirePeerPrincipal in front of it, it
// would be served to anyone, with no credential of any kind — and nothing else
// in the suite would notice, because every positive test still passes.
func TestEveryRecordedPeerRouteIsActuallyGated(t *testing.T) {
	var reached pmReached
	srv, _ := pmServer(t, pmSurface(t, &reached), &pmResolver{}, nil)

	recorded := srv.PeerRoutes()
	if len(recorded) != len(pmPeerPaths) {
		t.Fatalf("PeerRoutes() = %v, want the %d peer paths; an empty or short set would make the loop below vacuous",
			recorded, len(pmPeerPaths))
	}

	for _, path := range recorded {
		reached.calls = 0
		rec := httptest.NewRecorder()
		// No credential of ANY kind: no bearer token, no TLS, no certificate.
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))

		if rec.Code == http.StatusOK || rec.Code == http.StatusNoContent {
			t.Errorf("POST %s with NO credential of any kind = %d.\n"+
				"This path is in s.peerRoutes, which is what makes authMiddleware skip the bearer requirement for it. "+
				"It must therefore be wrapped in RequirePeerPrincipal — see mountPeerRoute, where the wrapping and the "+
				"recording happen in ONE function precisely so they cannot come apart.", path, rec.Code)
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with no credential = %d, want 403 from the peer gate", path, rec.Code)
		}
		if reached.calls != 0 {
			t.Errorf("POST %s reached the federation handler with no credential", path)
		}
	}
}

// ---------------------------------------------------------------------------
// ACK-3 — the peer bus id the binding rule uses comes from the CERTIFICATE
// ---------------------------------------------------------------------------

// pmAckCorrelationKey is the ONE correlation key pmSurface's outbox holds an
// obligation for, and it is owed to pmRemoteBus and to nobody else.
const pmAckCorrelationKey = pmRemoteBus + "-1"

// pmNullDurable is an OutboxDurableLog that accepts every write without a disk.
// The outbox needs one to enqueue at all; the ACK tests care about what the
// table SAYS, not about how it got there — internal/relay's own tests cover the
// durability.
type pmNullDurable struct{}

func (pmNullDurable) Write(wal.Entry) (wal.Committed, error) { return wal.Committed{}, nil }

// pmAckRequest is a well-formed peer ACK for pmAckCorrelationKey. extraHeaders
// lets a test add a header an attacker might hope the mount reads.
func pmAckRequest(t *testing.T, certs []*x509.Certificate, extraHeaders map[string]string) *http.Request {
	t.Helper()
	body, err := json.Marshal(relay.PeerAckRequest{
		ProtocolVersion:    relay.AckWireVersion,
		CorrelationKey:     pmAckCorrelationKey,
		Recipient:          pmLocalBus + ".bravo-1",
		Outcome:            "undeliverable",
		Class:              "no_route",
		EmittedAtUnixMilli: 1_700_000_000_000,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, relay.PeerAckPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	if certs != nil {
		req.TLS = &tls.ConnectionState{PeerCertificates: certs}
	}
	return req
}

// TestPeerAckBindsToTheCertificateResolvedBus is THE test for ACK-3's central
// constraint, and it is written to go RED for the one edit that would silently
// destroy the whole anti-forgery plane.
//
// relay.AuthorizePeerAck authorises DeriveJobID(peerBusID, correlationKey). The
// bus id it is given therefore decides WHOSE obligations a caller may settle. It
// must be the one RequirePeerPrincipal resolved from the presented CLIENT
// CERTIFICATE, and it must not be anything a request can carry.
//
// The frame has no field for it (internal/relay/ackframe.go, asserted there by
// reflection over the struct). This test covers the OTHER door: a header. A
// mount that read `X-Peer-Bus` — or any other request-supplied value — would
// still compile, would still pass every positive test in this file, and would let
// any peered bus settle any other peer's obligations.
//
// The asymmetry that makes it decidable: pmSurface's outbox owes pmRemoteBus one
// message and owes the OTHER bus nothing at all.
func TestPeerAckBindsToTheCertificateResolvedBus(t *testing.T) {
	const otherBus = "bus-mount-other"

	remoteCert := pmCert(t, pmRemoteBus)
	otherCert := pmCert(t, otherBus)
	res := &pmResolver{bound: map[buscert.Fingerprint]string{
		buscert.FingerprintOf(remoteCert): pmRemoteBus,
		buscert.FingerprintOf(otherCert):  otherBus,
	}}

	// 1. THE OWED PEER SUCCEEDS, and the id that reached the binding rule is the
	//    certificate-resolved one. Without this arm a mount that refused
	//    everything would pass arm 2.
	var reached pmReached
	srv, _ := pmServer(t, pmSurface(t, &reached), res, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, pmAckRequest(t, []*x509.Certificate{remoteCert}, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("the peer this bus owes got %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if reached.ackCalls != 1 {
		t.Fatalf("the ACK settle ran %d time(s), want exactly 1", reached.ackCalls)
	}
	if reached.ackPeerBusID != pmRemoteBus {
		t.Fatalf("the ACK route was told the peer was %q, want the certificate-resolved %q", reached.ackPeerBusID, pmRemoteBus)
	}

	// 2. THE FORGERY. A DIFFERENT, legitimately bound and legitimately
	//    authenticated peer presents a BYTE-IDENTICAL body and asks to settle the
	//    obligation this bus owes pmRemoteBus. It must be refused, and nothing
	//    may reach the durable settle.
	var forged pmReached
	srv2, _ := pmServer(t, pmSurface(t, &forged), res, nil)
	rec2 := httptest.NewRecorder()
	srv2.ServeHTTP(rec2, pmAckRequest(t, []*x509.Certificate{otherCert}, nil))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("a peer settling ANOTHER peer's obligation got %d, want %d", rec2.Code, http.StatusConflict)
	}
	if forged.ackCalls != 0 {
		t.Fatalf("a cross-route forgery reached the durable settle (peer recorded as %q); the mount is not supplying the certificate-resolved bus id",
			forged.ackPeerBusID)
	}

	// 3. AND THE SAME FORGERY WITH EVERY HEADER AN ATTACKER MIGHT HOPE IS READ.
	//    This is the mutation target: a mount that honoured any of these would
	//    turn arm 2 into a 200.
	for _, header := range []string{"X-Peer-Bus", "X-Peer-Bus-Id", "X-Bus-Id", "X-Forwarded-Bus", "Peer-Bus"} {
		var probed pmReached
		srv3, _ := pmServer(t, pmSurface(t, &probed), res, nil)
		rec3 := httptest.NewRecorder()
		srv3.ServeHTTP(rec3, pmAckRequest(t, []*x509.Certificate{otherCert}, map[string]string{header: pmRemoteBus}))
		if rec3.Code != http.StatusConflict {
			t.Errorf("%s: a peer naming another bus in a header got %d, want %d — the peer bus id must come from the CERTIFICATE and from nothing a request carries",
				header, rec3.Code, http.StatusConflict)
		}
		if probed.ackCalls != 0 {
			t.Errorf("%s: the header was honoured and the settle ran as %q", header, probed.ackPeerBusID)
		}
	}

	// 3b. AND THE FAIL-CLOSED BRANCH, reached by calling servePeerAck DIRECTLY
	//     with no principal in the context. It is unreachable through the mux —
	//     mountPeerRoute always wraps it in RequirePeerPrincipal — which is
	//     exactly why it needs asserting here: a branch nothing exercises is a
	//     branch that can be deleted or inverted with every test still green, and
	//     an empty peer id would derive a job id nobody owns, refusing every
	//     legitimate acknowledgement while LOOKING like a working guard.
	var direct pmReached
	srvD, _ := pmServer(t, pmSurface(t, &direct), res, nil)
	recD := httptest.NewRecorder()
	srvD.servePeerAck(recD, pmAckRequest(t, []*x509.Certificate{remoteCert}, nil))
	if recD.Code != http.StatusForbidden {
		t.Errorf("servePeerAck with NO peer principal in the context = %d, want 403; it must fail closed rather than hand the handler an empty bus id",
			recD.Code)
	}
	if direct.ackCalls != 0 {
		t.Errorf("servePeerAck ran the ACK handler with no principal, as peer %q", direct.ackPeerBusID)
	}

	// 4. NO CERTIFICATE AT ALL is 403 from the gate, and the ACK handler never
	//    runs. Without this the route could be gated by nothing and arms 1-3
	//    would still read as expected.
	var anon pmReached
	srv4, _ := pmServer(t, pmSurface(t, &anon), res, nil)
	rec4 := httptest.NewRecorder()
	srv4.ServeHTTP(rec4, pmAckRequest(t, nil, nil))
	if rec4.Code != http.StatusForbidden {
		t.Errorf("an anonymous ACK got %d, want 403", rec4.Code)
	}
	if anon.ackCalls != 0 {
		t.Error("the ACK settle ran for a caller that presented no certificate")
	}
}
