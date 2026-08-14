package httpapi

// Inbound PEER-BUS transport identity (RELAY-45).
//
// This file answers exactly one question, for the routes a peer bus calls:
//
//	which single ADJACENT BUS is at the other end of THIS TLS connection?
//
// The answer comes from the client certificate the peer presented and from
// nothing else. It is a TRANSPORT principal, and it is deliberately a different
// kind of thing from the AGENT principal authMiddleware attaches:
//
//	agent principal   a session token (an opaque server-side handle), naming a
//	                  fully-qualified <bus-id>.<agent-id>, obtained by enrolling
//	                  and completing a session (invariant 3).
//	peer principal    a TLS client certificate, naming a BUS id, bound by an
//	                  operator in the durable trust table (invariant 2 — a bus id
//	                  is the namespace half, never an agent id).
//
// NEITHER IS EVER ACCEPTED AS THE OTHER, and that is structural rather than
// remembered: they live under different context keys, they have different Go
// types, RequirePeerPrincipal never reads a token or a header, and it REMOVES
// any agent principal from the context it passes down (see requirePeerPrincipal
// for why removal rather than coexistence).
//
// WHAT IS NOT HERE: no route. RELAY-20 mounts the peer surface and decides which
// paths sit behind this wrapper; this file supplies the wrapper, the context
// accessor and the fail-closed default, so that mounting a route behind it is
// the only thing left to do.

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"

	"github.com/dodgymike/agent-bus/internal/buscert"
)

// InboundPeerPrincipals resolves a presented client certificate to the one
// adjacent bus principal it names.
//
// It is satisfied by *relay.PeerStore, and it is an INTERFACE for the reason
// every other collaborator here is one: this package must be constructible in a
// test without a data directory, and it must not learn how the server assembled
// its federation configuration. It is deliberately the narrowest possible shape
// — one method, one argument, one answer — so that nothing in this package can
// reach the route table, the base URLs or the signing keys next to it.
//
// THE IMPLEMENTATION MUST FAIL CLOSED. An error means the connection names
// nobody; there is no "unknown, so allow", no trust-on-first-use and no default
// principal. See relay.PeerStore.InboundPeerPrincipal for the six inputs such an
// implementation must never read — chief among them the OUTBOUND next-hop
// certificate pin, which names a certificate travelling the other way.
type InboundPeerPrincipals interface {
	InboundPeerPrincipal(cert *x509.Certificate) (string, error)
}

// PeerPrincipal is the authenticated identity of the ADJACENT BUS on a
// connection: the answer to "who is speaking", never "where is this going" and
// never "who originally sent this".
//
// It names a BUS, so BusID is a bare bus id and NOT a fully-qualified
// `<bus-id>.<agent-id>` (invariant 2 — ids.ValidateBusID refuses the '.' that
// would make it one). A handler that needs the original SENDER of a relayed
// message must take it from the signed envelope and verify it against the pinned
// origin bus signing key; the peer principal proves only which bus handed us the
// bytes, which is exactly what loop prevention, next-hop authorisation and rate
// attribution need.
type PeerPrincipal struct {
	// BusID is the adjacent bus's server-minted id, in its canonical spelling as
	// the operator recorded it.
	BusID string

	// CertFingerprint is the fingerprint of the leaf that authenticated this
	// connection — sha256 over the DER exactly as it arrived. It is carried so a
	// handler and an operator can name the credential in a log line without
	// recomputing it a second (possibly different) way. It is PUBLIC data: a
	// certificate fingerprint is derivable by anyone who completes a handshake.
	CertFingerprint buscert.Fingerprint
}

const ctxKeyPeerPrincipal ctxKey = 2

// noAgentPrincipal is what RequirePeerPrincipal puts under ctxKeyPrincipal to
// REMOVE an agent principal from a peer request's context.
//
// A context value cannot be deleted, only shadowed, so the removal is spelled as
// a value of a type PrincipalFromContext's assertion cannot accept. It is a
// named type rather than nil so that the shadowing is visible to anyone reading
// a context dump, and so it can never be confused with "no middleware ran".
type noAgentPrincipal struct{}

// PeerPrincipalFromContext returns the adjacent-bus identity attached by
// RequirePeerPrincipal, and whether there was one.
//
// The principal is placed in the context ONLY by RequirePeerPrincipal, and ONLY
// after InboundPeerPrincipals resolved a presented client certificate to exactly
// one bus. A handler may therefore treat it as the authenticated transport
// subject of the request, and must NEVER take a peer identity from a header, a
// query parameter or a request body — those are client-supplied claims, and the
// server is authoritative on every id (invariant 1).
//
// A HANDLER THAT HAS A CLAIMED PEER IDENTITY IN THE BODY MUST CROSS-CHECK IT
// AGAINST THIS VALUE, never use it instead: that is invariant 11's rule that a
// credential presented over a connection belonging to a different principal is
// rejected, at bus scope.
//
// ok == false means the request did not come through RequirePeerPrincipal, which
// for a handler that needs a peer principal is a wiring bug and must be treated
// as a refusal, never as "no restriction applies".
func PeerPrincipalFromContext(ctx context.Context) (PeerPrincipal, bool) {
	if ctx == nil {
		return PeerPrincipal{}, false
	}
	p, ok := ctx.Value(ctxKeyPeerPrincipal).(PeerPrincipal)
	return p, ok
}

// PeerBusIDFromContext returns the adjacent bus's id, or "" when the request
// carried no peer principal. See PeerPrincipalFromContext.
func PeerBusIDFromContext(ctx context.Context) string {
	p, ok := PeerPrincipalFromContext(ctx)
	if !ok {
		return ""
	}
	return p.BusID
}

// peerPrincipalRefusal is the ONE thing a refused caller ever learns.
//
// It is a single fixed string for every failure — no resolver configured, no
// TLS, no certificate, an unknown fingerprint, a withdrawn binding, an ambiguous
// binding — for authMiddleware's reason: distinguishing them is an enumeration
// oracle that would let anyone with a certificate probe which buses this bus has
// peered with, and whether a particular binding had been revoked. The LOG says
// precisely which case it was.
const peerPrincipalRefusal = "this connection is not an authorised peer bus"

// errNoPeerResolver is the wiring failure: a route was mounted behind
// RequirePeerPrincipal on a server built without an InboundPeerPrincipals. It is
// a distinct sentinel only so the log line can name it; the client is told the
// same thing as everyone else.
var errNoPeerResolver = errors.New("httpapi: this server was built without an inbound peer principal resolver")

// RequirePeerPrincipal wraps a handler so it runs ONLY for a request that
// arrived over a TLS connection whose client certificate is bound to exactly one
// adjacent bus principal, and attaches that principal to the request context.
//
// It is the inbound peer-bus gate. RELAY-20 mounts the peer routes behind it.
//
// # THE ORDER OF THE CHECKS IS THE CONTRACT
//
//  1. A resolver must be configured. Without one, EVERY request is refused —
//     there is no permissive default, because a peer route that admits everyone
//     when its configuration is missing is worse than one that admits nobody.
//  2. The request must have arrived over TLS. r.TLS is nil on a plaintext
//     connection, which this server never serves (invariant 11) and which a test
//     harness or a future in-process listener could still produce.
//  3. A client certificate must have been presented. THE LISTENER ONLY REQUESTS
//     ONE (tls.RequestClientCert), so an EMPTY r.TLS.PeerCertificates is the
//     ordinary case for every agent connection, not an exotic one — a consumer
//     that reached straight for index 0 would panic on almost every request, and
//     net/http would recover it per connection so it presented as a mysterious
//     dropped request rather than a crash.
//  4. INDEX [0] ONLY, NEVER ITERATE. The peer controls the whole chain it sends,
//     while the handshake's CertificateVerify proves possession of the LEAF's
//     private key alone. A gate that SEARCHED the chain for a bound fingerprint
//     would be spoofed by anyone who appended the victim's public certificate at
//     index 1. Extra chain entries are simply ignored; they authorise nothing.
//  5. THE LEAF MUST BE IN DATE (added by RELAY-20, the first thing that
//     authorises on a client certificate). tls.RequestClientCert does no chain
//     verification, so nothing else on this side of the connection has ever
//     looked at NotBefore or NotAfter: an expired certificate completes the
//     handshake and arrives here exactly like a fresh one. Without this check a
//     durable binding would outlive the credential it names, and expiry — the
//     only automatic bound on a leaked peer key — would mean nothing on this
//     surface. crypto/x509 returns the verdict; see checkClientCertValidity in
//     peermount.go for why it is never a local date comparison (invariant 9).
//  6. The leaf resolves through the durable binding, or the request is refused.
//     No fallback, no second lookup, no principal on any error path.
//
// # WHAT IT REFUSES WITH, AND WHY IT IS NOT 401
//
// 403, with no WWW-Authenticate challenge. A 401 MUST carry one (RFC 7235), and
// the only challenge this server speaks is `Bearer` — which would invite a
// refused peer to retry with a session token, i.e. it would advertise exactly
// the credential confusion this gate exists to prevent. The credential here is
// the TLS client certificate: it is chosen when the connection is established
// and cannot be supplied by retrying with a different header, so "forbidden for
// this connection" is the honest answer.
//
// # THE AGENT PRINCIPAL IS REMOVED, NOT MERELY IGNORED
//
// A peer bus is also an enrolled principal on the buses it peers with, so a peer
// request may well carry a valid session token, and authMiddleware may already
// have attached the resulting agent principal. This wrapper does not read it —
// but leaving it in the context would let a peer handler pick up an AGENT
// identity and act on it as if it had authorised the peer request, which is
// precisely "a session credential accepted as a peer-bus credential". So the
// context handed downstream has the agent principal SHADOWED OUT: on a peer
// route there is exactly one principal, and it is the transport one.
//
// # THIS MUST BE THE INNERMOST AUTH-BEARING WRAPPER (reviewer gate, RELAY-45)
//
// The shadowing is ORDER-DEPENDENT, and it fails SILENTLY in the wrong order,
// which is why it is stated here rather than left to be inferred:
//
//	authMiddleware(RequirePeerPrincipal(h))   CORRECT — the shadow is applied
//	                                          last, so h sees the peer principal
//	                                          and no agent principal.
//	RequirePeerPrincipal(authMiddleware(h))   WRONG — authMiddleware runs INSIDE
//	                                          and re-attaches the agent principal
//	                                          over the shadow. Every positive test
//	                                          still passes; the property is gone.
//
// So whatever mounts a peer route must put this wrapper CLOSEST to the handler.
//
// # AND IT IS ONE FACTOR, WHICH IS A DECISION RELAY-20 OWNS, NOT THIS FILE
//
// Passing this gate proves possession of a bound certificate's private key and
// nothing else. Invariant 11 requires mTLS and the session token to BOTH be
// present and to CROSS-CHECK, and invariant 3 says invites are the only way onto
// the bus "including for peer buses" — so a peer route protected by this wrapper
// ALONE is weaker than the invariants ask for. That is not a defect here: this
// task supplies the transport half, which did not exist at all before it. RELAY-20
// mounts the routes and must decide how the two factors are combined; it must not
// read a passing gate as a finished authorisation.
func (s *Server) RequirePeerPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.peerPrincipals == nil {
			s.log.Warn("peer request refused: no inbound peer principal resolver is configured, so this bus can authorise no peer bus",
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"err", errNoPeerResolver,
			)
			s.writePeerForbidden(w, r)
			return
		}

		if r.TLS == nil {
			s.log.Warn("peer request refused: it did not arrive over TLS, so it carries no transport identity at all",
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
			)
			s.writePeerForbidden(w, r)
			return
		}

		// CHECK THE SLICE IS NON-EMPTY FIRST, THEN INDEX [0] ONLY.
		if len(r.TLS.PeerCertificates) == 0 {
			s.log.Debug("peer request refused: no client certificate was presented",
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
			)
			s.writePeerForbidden(w, r)
			return
		}
		leaf := r.TLS.PeerCertificates[0]

		// THE VALIDITY WINDOW, BEFORE THE BINDING IS CONSULTED (RELAY-20).
		// Inside this gate rather than beside it, so no present or future mount
		// can reach the binding with an out-of-date certificate. It judges
		// DATES ONLY and authorises nothing; the identity is still the
		// fingerprint lookup below. Refused with the SAME fixed string as every
		// other case here -- an anonymous caller must not be able to tell
		// "expired" from "unknown", which would say whether this bus had ever
		// bound that certificate.
		if err := checkClientCertValidity(leaf, s.now()); err != nil {
			s.log.Info("peer request refused: the presented client certificate is outside its own validity window",
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"client_cert_sha256", buscert.FingerprintOf(leaf).String(),
				"err", err,
			)
			s.writePeerForbidden(w, r)
			return
		}

		busID, err := s.peerPrincipals.InboundPeerPrincipal(leaf)
		if err != nil {
			// Info, not Debug: a certificate that does not resolve is either a
			// peering the operator has not finished configuring or someone
			// trying one, and an operator should see the second by default. The
			// fingerprint is logged because it is PUBLIC and because it is the
			// exact value an operator needs to bind — a refusal that does not
			// name it costs an afternoon.
			s.log.Info("peer request refused: the presented client certificate resolves to no single adjacent bus",
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"client_cert_sha256", buscert.FingerprintOf(leaf).String(),
				"err", err,
			)
			s.writePeerForbidden(w, r)
			return
		}

		principal := PeerPrincipal{BusID: busID, CertFingerprint: buscert.FingerprintOf(leaf)}
		s.log.Debug("peer request authenticated",
			"request_id", RequestIDFromContext(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"peer_bus", principal.BusID,
			"client_cert_sha256", principal.CertFingerprint.String(),
		)

		ctx := context.WithValue(r.Context(), ctxKeyPeerPrincipal, principal)
		// See the doc comment: the agent principal is shadowed out rather than
		// left to be picked up by a peer handler.
		ctx = context.WithValue(ctx, ctxKeyPrincipal, noAgentPrincipal{})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// writePeerForbidden answers 403 with the standard error body and the one fixed
// reason. It carries NO WWW-Authenticate header — see RequirePeerPrincipal.
func (s *Server) writePeerForbidden(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusForbidden, ErrorResponse{Error: peerPrincipalRefusal})
}
