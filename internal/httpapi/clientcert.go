package httpapi

import (
	"context"
	"crypto/x509"
	"net/http"

	"github.com/dodgymike/agent-bus/internal/buscert"
)

// ClientCertificate is the client certificate an ORDINARY AGENT connection
// presented, reduced to the one thing anything downstream may act on: the
// fingerprint the enrolment binding is keyed by (MTLS-BIND).
//
// # IT IS A TRANSPORT FACT AND IT AUTHORISES NOTHING
//
// Read the name literally. This is not a principal and must never be used as
// one. Its presence means only that the TLS handshake proved the peer holds the
// private key of a certificate with this fingerprint, and that the certificate
// was in date at that moment. It says nothing about WHO that is: an unenrolled
// stranger with a self-signed certificate reaches this struct exactly as an
// enrolled agent does, because the listener is tls.RequestClientCert and there
// is no CA anywhere in this design (invariant 11).
//
// The AUTHORISED fact is the enrolment binding, and it lives in the roster:
// auth.Service.AgentIDForClientCertificate turns this fingerprint into an agent
// id or refuses. A handler that treats a non-empty ClientCertificate as an
// identity has skipped the only step that makes it one — which is precisely the
// "admitted but unauthorised" gap MTLS-CLIENTAUTH left open on purpose and this
// task closes.
type ClientCertificate struct {
	// Fingerprint is sha256 over the leaf's DER exactly as it arrived —
	// buscert.FingerprintOf, never a second implementation and never a
	// re-marshalled certificate (invariant 9: one construction, one answer). It
	// is PUBLIC data: anyone who completes a handshake with this peer can
	// compute it, which is why it is safe to log.
	Fingerprint buscert.Fingerprint

	// Leaf is the parsed certificate at index [0] of the presented chain, for a
	// caller that needs to say something about it in a log line. It is NEVER the
	// basis of an authorisation decision — the fingerprint is. In particular
	// nothing may read a name, a SAN or an EKU out of it and treat that as an
	// identity claim: those fields are chosen entirely by whoever generated the
	// certificate, which on this bus is the client itself.
	Leaf *x509.Certificate
}

// ctxKeyClientCert is 3.
//
// The task text said the next free value was 2. IT IS NOT — peerprincipal.go
// took 2 for ctxKeyPeerPrincipal, alongside 0 (request id, middleware.go) and 1
// (agent principal, authmw.go). The value was confirmed against the tree rather
// than taken from the task, because these constants are compared by VALUE and a
// collision does not fail to compile: two keys with the same value silently
// shadow each other's context entries, so the wrong type comes back out and the
// type assertion in the accessor turns it into "no value present" — a
// fail-OPEN-looking absence with nothing to notice it. If you add a fourth, read
// all three of the files above first.
const ctxKeyClientCert ctxKey = 3

// ClientCertificateFromContext returns the client certificate presented on this
// request's connection, and whether there was a usable one.
//
// ok == false means one of: the request did not arrive over TLS, no client
// certificate was presented, or the presented leaf was outside its own validity
// window. THOSE THREE ARE DELIBERATELY NOT DISTINGUISHED HERE — every one of
// them means "there is no transport identity to check against", which is the
// only distinction a caller may act on. A caller that needs to explain WHICH to
// an operator reads the server log, where WithClientCertificate has already said
// so specifically.
//
// A caller that requires a certificate must treat ok == false as a REFUSAL, and
// never as "no constraint applies". On this build no route requires one — see
// WithClientCertificate.
func ClientCertificateFromContext(ctx context.Context) (ClientCertificate, bool) {
	if ctx == nil {
		return ClientCertificate{}, false
	}
	c, ok := ctx.Value(ctxKeyClientCert).(ClientCertificate)
	return c, ok
}

// ClientCertFingerprintFromContext returns just the fingerprint, and whether
// there was one. It is the form the enrolment handler needs, and it exists so
// that the common caller cannot accidentally end up holding the Leaf and reading
// an identity claim out of it.
func ClientCertFingerprintFromContext(ctx context.Context) (buscert.Fingerprint, bool) {
	c, ok := ClientCertificateFromContext(ctx)
	if !ok {
		return buscert.Fingerprint{}, false
	}
	return c.Fingerprint, true
}

// WithClientCertificate attaches the connection's client certificate to the
// request context, for the routes that care. It is the middleware clause of
// MTLS-BIND: it plumbs r.TLS into internal/httpapi so a handler can bind or
// cross-check a certificate without reaching for the transport itself.
//
// # IT ADMITS EVERY REQUEST. IT IS NOT A GATE.
//
// This middleware NEVER refuses anything, and that is the decision, not an
// omission. The listener is tls.RequestClientCert (cmd/agent-bus/tlslisten.go),
// which REQUESTS a client certificate and never REQUIRES one, so a connection
// carrying none is the ORDINARY case: /healthz, /v1/info, the container's own
// healthcheck and every client that has not yet grown a keypair
// (MTLS-CLIENTCERT is still open) all arrive without one. Refusing here would
// invent "a client certificate is mandatory" in the middleware layer while the
// transport layer says it is optional — the same requirement enforced in two
// places that disagree, which is how it ends up enforced in neither — and it
// would take the bus's own health probe down with it.
//
// The migration-safe target is PER-AGENT enforcement: once an agent HAS a
// binding, its requests must present it. That needs bindings to exist first,
// which is what this task creates, and the enforcement is MTLS-CROSSCHECK's.
//
// # THE ORDER OF THE CHECKS IS THE CONTRACT
//
//  1. TLS or nothing. r.TLS is nil on a plaintext connection, which this server
//     never serves (invariant 11) but a test harness or an in-process listener
//     can still produce.
//  2. A certificate must have been presented. An EMPTY PeerCertificates is the
//     ordinary case here, not an exotic one — a consumer that reached straight
//     for index 0 would panic on most requests, and net/http would recover it
//     per connection, so it would present as mysteriously dropped requests
//     rather than as a crash.
//  3. INDEX [0] ONLY, NEVER ITERATE THE CHAIN. The client controls every
//     certificate it sends, while the handshake's CertificateVerify proves
//     possession of the LEAF's private key ALONE. Searching the chain for a
//     certificate that happens to be bound would be spoofed by anyone who
//     appended the victim's PUBLIC certificate at index 1 — public data, freely
//     obtainable — and that single mistake would hand an attacker the victim's
//     agent id. Extra chain entries are ignored; they authorise nothing.
//  4. THE LEAF MUST BE IN DATE, BEFORE THE FINGERPRINT IS PUBLISHED. crypto/tls
//     proves possession of the private key and does NOT check dates, so an
//     expired certificate completes the handshake and arrives here exactly like
//     a fresh one. Without this, expiry — the only automatic bound on a leaked
//     client key — would mean nothing on the agent plane, and enrolment would
//     mint a durable binding that outlives the credential it names. It is
//     crypto/x509's verdict and never a local date comparison (invariant 9);
//     RELAY-20 closed the identical hole on the peer plane and this uses the
//     same helper, checkClientCertValidity, so the two planes cannot drift.
//
// A leaf that fails any step attaches NOTHING rather than attaching something
// marked invalid. An invalid-but-present value is the shape that gets read past:
// somebody checks the presence and forgets the flag. Absence cannot be read
// past.
//
// # WHY IT SITS OUTSIDE authMiddleware
//
// It is mounted OUTSIDE the authentication middleware so the fact is available
// on the UNAUTHENTICATED routes too — enrolment is one, and enrolment is the
// route that creates the binding, so a certificate that only became visible
// after authentication could never be bound to anything.
//
// It does not interact with RequirePeerPrincipal and does not need to. That
// wrapper shadows out the AGENT PRINCIPAL, because an agent principal on a peer
// route would be one principal accepted as another. This value is not a
// principal (see ClientCertificate) — on a peer route it simply describes the
// same certificate RequirePeerPrincipal already authorised, which is the truth
// and authorises nothing extra.
func (s *Server) WithClientCertificate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaf := presentedClientLeaf(r)
		if leaf == nil {
			next.ServeHTTP(w, r)
			return
		}

		if err := checkClientCertValidity(leaf, s.now()); err != nil {
			// Info, not Debug: a client presenting an out-of-date certificate is
			// either an agent whose credential quietly expired — which it cannot
			// see from the outside, because the request SUCCEEDS unbound — or
			// someone trying a stale one, and an operator should see both by
			// default. The fingerprint is logged because it is PUBLIC and is the
			// exact value the operator needs to correlate with a roster binding.
			s.log.Info("a client certificate was presented but is outside its own validity window; it is IGNORED and the request continues WITHOUT a transport identity, so it can bind nothing and satisfies no cross-check",
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"client_cert_sha256", buscert.FingerprintOf(leaf).String(),
				"err", err,
			)
			next.ServeHTTP(w, r)
			return
		}

		cert := ClientCertificate{Fingerprint: buscert.FingerprintOf(leaf), Leaf: leaf}
		ctx := context.WithValue(r.Context(), ctxKeyClientCert, cert)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// enrolCertFingerprint renders the request's client-certificate fingerprint in
// the form auth.EnrolRequest takes: a pointer, nil when there is none.
//
// The conversion buscert.Fingerprint -> [32]byte is a NAMED-TYPE conversion and
// not a re-hash: both are [32]byte and the bytes are carried through unchanged,
// so there is exactly one fingerprint construction in the process
// (buscert.FingerprintOf) and no second answer for the same certificate.
// internal/auth takes the unnamed array rather than importing internal/buscert
// on purpose — the auth package stores a digest and has no business knowing how
// certificates are parsed.
//
// It returns a pointer to a FRESH copy. Handing back a pointer into anything the
// context holds would let the callee mutate a value another handler on the same
// request could still read.
func enrolCertFingerprint(r *http.Request) *[32]byte {
	fp, ok := ClientCertFingerprintFromContext(r.Context())
	if !ok {
		return nil
	}
	out := [32]byte(fp)
	return &out
}

// presentedClientLeaf returns the leaf certificate the client presented, or nil
// when the request carried none.
//
// It is the ONE place index [0] is taken, so the never-iterate-the-chain rule of
// WithClientCertificate step 3 has a single site to audit rather than one per
// caller.
func presentedClientLeaf(r *http.Request) *x509.Certificate {
	if r == nil || r.TLS == nil {
		return nil
	}
	if len(r.TLS.PeerCertificates) == 0 {
		return nil
	}
	return r.TLS.PeerCertificates[0]
}
