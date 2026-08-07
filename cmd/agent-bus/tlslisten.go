package main

// The TLS half of the listener (MTLS-LISTENER): turning the bus's already-loaded
// certificate and key (cmd/agent-bus/buscert.go, internal/buscert) into the
// *tls.Config that wraps the one and only listener this process opens.
//
// # THERE IS NO PLAINTEXT LISTENER, AND NO FLAG THAT MAKES ONE
//
// Invariant 11: every HTTP surface — client and bus-to-bus relay — is served
// over TLS, and the server REFUSES TO START rather than fall back to plaintext.
// That refusal is structural here rather than conditional: run() has exactly one
// net.Listen, its result is handed to tls.NewListener before anything can Serve
// on it, and busTLSConfig below returns an error rather than a usable-looking
// config when the key material is not usable. There is no branch to take, so
// there is no branch to take by accident.
//
// The reason it is worth stating that plainly: the session token is a BEARER
// credential. On a plaintext listener an on-path observer reads it, replays it,
// or kills a pending challenge, and every other authentication control in this
// repo is decoration. "Loopback by default" bounds who is on the path; it does
// not make the path safe, and the loopback default therefore stays exactly as it
// was (-listen 127.0.0.1:8080) rather than being treated as an alternative to
// this file.
//
// # SCOPE: THIS DOES NOT REQUIRE A CLIENT CERTIFICATE
//
// Stated as loudly as the paragraph above, because it is the half most likely to
// be "completed" by a well-meaning later change. ClientAuth is pinned to
// tls.NoClientCert BELOW, deliberately, and moving it is MTLS-CLIENTAUTH's job —
// and MTLS-CLIENTAUTH may not land before MTLS-CLIENTCERT, which is the task
// that teaches the CLIENT to generate and present a certificate at all.
//
// This ordering is not a preference. This repo has already shipped server-side
// enforcement ahead of client-side capability once: signature checking landed
// before the client could sign, and every send failed with curl exit 7 until it
// was reverted. A bus that demanded a client certificate today would refuse every
// agent in the fleet at the handshake, before any route, log line or error
// message the agent could act on. Mutual TLS is the design (invariant 11) and it
// arrives in the order the two ends can actually speak it.
//
// # NEVER WRITE YOUR OWN CRYPTO (invariant 9)
//
// Everything below is stdlib crypto/tls, CONFIGURED. No construction, no
// primitive assembly, no hand-rolled anything — and no InsecureSkipVerify, which
// is permitted in exactly one file in this repo (client/pin.go, paired with
// VerifyPeerCertificate) and must never appear here. A server does not verify
// itself, so there is nothing here that could even want it.

import (
	"crypto/tls"
	"fmt"

	"github.com/dodgymike/agent-bus/internal/buscert"
)

// busTLSConfig builds the server-side TLS configuration from the bus's loaded
// certificate material.
//
// It returns an error — never a degraded config — when the material cannot serve
// TLS. The caller must treat that as FATAL: with no plaintext listener there is
// nothing to fall back TO, and a bus that came up anyway would be a bus serving
// nothing while looking healthy.
//
// The checks are belt-and-braces. internal/buscert.LoadOrCreate has already
// refused a missing, damaged, world-readable or expired file, and it is the one
// place that validation belongs. What is checked here is the narrower property
// this file depends on and buscert's signature does not promise: that the
// Material really did yield a usable tls.Certificate. A zero tls.Certificate is
// accepted by tls.NewListener without complaint and fails at the first
// handshake, per connection, with no startup signal at all — exactly the silent
// half-outage the refusal exists to prevent.
func busTLSConfig(material *buscert.Material) (*tls.Config, error) {
	if material == nil {
		// Unreachable from run(): openBusCertMaterial returns a non-nil Material
		// or a non-nil error. Checked so a future caller gets a named refusal
		// rather than a nil dereference on the serving path.
		return nil, fmt.Errorf("no bus certificate material was loaded")
	}
	cert := material.TLSCertificate()
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("the loaded bus certificate %q carries no certificate chain; the bus cannot serve TLS and there is no plaintext listener to fall back to", material.CertPath())
	}
	if cert.PrivateKey == nil {
		return nil, fmt.Errorf("the loaded bus certificate %q has no private key from %q; the bus cannot serve TLS and there is no plaintext listener to fall back to", material.CertPath(), material.TLSKeyPath())
	}

	return &tls.Config{
		// The bus's own self-signed certificate, presented on every handshake.
		// Clients do not chain it to a CA — there is none, by design (E6) — they
		// PIN its fingerprint, which the bus logs at startup as
		// bus_cert_fingerprint and the invite blob carries.
		Certificates: []tls.Certificate{cert},

		// TLS 1.2 floor, matching client/pin.go's pinnedTLSConfig exactly. The
		// two ends' floors are deliberately the same value: a server floor above
		// the client's would be a handshake failure with no useful message at
		// either end, and this repo's own client is not the only consumer — an
		// operator's curl --cacert against /healthz has to work too.
		//
		// 1.3 is negotiated whenever both ends offer it, which for every Go
		// client in this repo is always.
		MinVersion: tls.VersionTLS12,

		// NO CLIENT CERTIFICATE IS REQUESTED OR REQUIRED. See this file's header:
		// requiring one before MTLS-CLIENTCERT ships locks out every agent at the
		// handshake. Written explicitly rather than left as the zero value so the
		// choice is visible to the next reader and shows up in a diff when it
		// legitimately changes (MTLS-CLIENTAUTH).
		ClientAuth: tls.NoClientCert,

		// Pinned to HTTP/1.1 because this listener is wrapped by tls.NewListener
		// and served by http.Server.Serve, which — unlike ServeTLS — does not
		// configure HTTP/2. Advertising only what is actually served keeps ALPN
		// honest: a client offering h2 AND http/1.1 negotiates http/1.1 here
		// instead of being left to infer it from an empty ALPN result.
		//
		// Stated because it is the one case this is not merely cosmetic: a client
		// offering ONLY h2 now gets a handshake failure (no_application_protocol)
		// rather than a silent fallback. That is the correct outcome — it would
		// otherwise send an HTTP/2 preface to an HTTP/1.1 server and fail later
		// and less legibly — but it IS a refusal, not a downgrade.
		NextProtos: []string{"http/1.1"},

		// NO SESSION TICKETS. Added on the security gate's finding (L1), and it is
		// defence in depth rather than a fix for a live bug: crypto/tls does NOT
		// invoke VerifyPeerCertificate on a RESUMED handshake, and for this
		// project's clients the entire certificate pin lives in that callback
		// (client/pin.go). No client in this tree sets a ClientSessionCache today,
		// so nothing resumes and nothing is bypassed — but "nothing resumes" is a
		// property of every client, enforced only by a guard test in client/, and
		// one latency-motivated cache away from being false. Refusing to issue
		// tickets makes the pin unbypassable from THIS end regardless of what any
		// client does later, which is the end we control.
		SessionTicketsDisabled: true,
	}, nil
}

// tlsVersionName renders a crypto/tls version constant for the startup summary.
//
// It exists so main.go can DERIVE the logged floor from the config it actually
// serves rather than restate it as a literal beside it. The reviewer gate showed
// why that distinction is not pedantry: with a literal, changing MinVersion left
// the startup line reporting the old value and every test still green, under a
// comment claiming the line states a fact about the listener.
//
// An unrecognised version renders as its hex value rather than as "unknown": the
// operator reading this line needs something they can look up, and a made-up
// friendly name for a constant this function has not been taught is worse than
// the number.
func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "1.0"
	case tls.VersionTLS11:
		return "1.1"
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS13:
		return "1.3"
	case 0:
		// crypto/tls treats zero as "the stdlib default", which is a moving
		// target across Go releases. Saying so is more useful than printing a
		// number that names no version.
		return "default"
	default:
		return fmt.Sprintf("%#04x", v)
	}
}

// clientAuthName renders a tls.ClientAuthType for the startup summary.
//
// "none" and everything else are deliberately NOT collapsed. The whole reason
// this field is logged is so an operator can tell at a glance whether the bus
// requires a client certificate, and the day MTLS-CLIENTAUTH changes that, the
// summary must change with it rather than keep reporting the value someone typed
// into a log call months earlier.
func clientAuthName(t tls.ClientAuthType) string {
	switch t {
	case tls.NoClientCert:
		return "none"
	case tls.RequestClientCert:
		return "requested"
	case tls.RequireAnyClientCert:
		return "required-any"
	case tls.VerifyClientCertIfGiven:
		return "verified-if-given"
	case tls.RequireAndVerifyClientCert:
		return "required-and-verified"
	default:
		return fmt.Sprintf("unrecognised(%d)", int(t))
	}
}
