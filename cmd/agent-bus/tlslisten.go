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
// # SCOPE: A CLIENT CERTIFICATE IS REQUESTED, AND NEVER REQUIRED
//
// Since MTLS-CLIENTAUTH (2026-08-14) ClientAuth is tls.RequestClientCert. The
// listener ASKS every client for a certificate, so a client that has one puts it
// on the connection where the application layer can see it in
// r.TLS.PeerCertificates — and a client that has none still completes the
// handshake and is served exactly as before.
//
// REQUESTED, NOT REQUIRED, is the whole judgement and it is deliberate. This repo
// has already shipped server-side enforcement ahead of client-side capability
// once: signature checking landed before the client could sign, and every send
// failed with curl exit 7 until it was reverted. tls.RequireAnyClientCert today
// would refuse, at the handshake and before any route, log line or error message
// they could act on: every agent whose identity directory predates
// MTLS-CLIENTCERT, `agent-bus healthcheck` (which presents no certificate, and is
// what Docker's HEALTHCHECK branches on), and every operator probe of /healthz.
// Mutual TLS is the design (invariant 11) and it arrives in the order the two
// ends can actually speak it; the requirement is REACHED by binding certificates
// to agent ids (MTLS-BIND) and then refusing unbound principals per-route, not by
// slamming the handshake shut first.
//
// tls.VerifyClientCertIfGiven is not the middle ground it reads as — it is the
// one setting that would break the exact case this task exists to enable. It sits
// at or above crypto/tls's verification threshold, so the stdlib runs
// certs[0].Verify against ClientCAs (see processCertsFromClient). There is no CA
// in this design and ClientCAs is nil, which means the system roots; a
// self-signed agent or peer-bus certificate chains to nothing in them and the
// handshake is ABORTED. It would admit every certificate-less client and reject
// every client that actually presented one — precisely backwards.
//
// What RequestClientCert gives up, exactly: nothing is authorised at handshake
// time. It does NOT give up proof of possession — a client that sends a
// certificate must also send a CertificateVerify signed with that certificate's
// private key, and crypto/tls verifies it in EVERY mode (neither the TLS 1.2 nor
// the TLS 1.3 server path gates that on the ClientAuth level, which the security
// gate confirmed against the stdlib source). So this listener gets exactly the
// same proof of possession RequireAndVerifyClientCert would. A certificate on the
// connection proves its holder has the key; it does not prove WHO they are, and
// nothing here may be read as if it did. Resolving a fingerprint to an agent
// (MTLS-BIND, MTLS-CROSSCHECK) or to a peer bus (RELAY-20) is application-layer
// work and is deliberately absent from this file.
//
// TWO COSTS THAT ARE NOT ZERO, recorded rather than glossed:
//
//   - PRIVACY, on TLS 1.2 only. Asking for a certificate puts the client's
//     Certificate message on the wire, and in TLS 1.2 it is not encrypted — a
//     passive observer gains a stable, per-agent correlatable identifier (the
//     leaf's public key) that did not exist under NoClientCert. TLS 1.3 encrypts
//     it, no MaxVersion is set here, and every Go client in this repo negotiates
//     1.3, so the residue is TLS-1.2-only peers. The certificate's CN is a fixed
//     descriptive string, so no agent NAME leaks — the key does.
//   - MEMORY, from unauthenticated peers. crypto/tls caps a handshake message at
//     64 KiB, and the parsed chain is retained for the connection's lifetime, so
//     one connection can hold roughly half a megabyte of certificates for as long
//     as it stays open. Bounded by the loopback default listen address rather
//     than by anything here.
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
	"crypto/x509"
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

		// A client certificate is REQUESTED and never REQUIRED (MTLS-CLIENTAUTH).
		// See this file's header for why the two neighbouring values are both
		// wrong: RequireAnyClientCert locks out every certificate-less client at
		// the handshake, and VerifyClientCertIfGiven rejects every certificate
		// that IS presented, because it chain-verifies against a CA that does not
		// exist.
		ClientAuth: tls.RequestClientCert,

		// The replacement for the chain verification RequestClientCert does not
		// do. There is no CA to chain to and there never will be (invariant 11),
		// so this is where a client certificate is judged — and what it judges is
		// deliberately narrow. Read admitClientCertificate before assuming a
		// connection that got past it was authorised: it was not.
		//
		// It runs on EVERY handshake, including those carrying no certificate,
		// because crypto/tls invokes this callback from processCertsFromClient
		// whenever a certificate was requested.
		//
		// INCLUDING RESUMED ONES, and that is worth stating because the CLIENT
		// side is the opposite and the two are easily confused. client/pin.go
		// correctly warns that a resuming CLIENT does not re-verify the server's
		// certificate. A resuming SERVER does: both doResumeHandshake (TLS 1.2)
		// and checkForResumption (TLS 1.3) replay the session's cached
		// certificates through processCertsFromClient, which calls this callback
		// unconditionally. So this callback does NOT depend on
		// SessionTicketsDisabled below — checked against crypto/tls rather than
		// assumed by symmetry with the client.
		VerifyPeerCertificate: admitClientCertificate,

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
		// defence in depth rather than a fix for a live bug: on the CLIENT SIDE
		// crypto/tls does NOT invoke VerifyPeerCertificate on a RESUMED handshake,
		// and for this project's clients the entire certificate pin lives in that
		// callback (client/pin.go). The qualifier is load-bearing and was added by
		// MTLS-CLIENTAUTH: the SERVER side is the opposite (see
		// VerifyPeerCertificate above), so this setting justifies itself by the
		// client's behaviour alone and must not be read as what keeps this
		// listener's own callback running.
		// No client in this tree sets a ClientSessionCache today,
		// so nothing resumes and nothing is bypassed — but "nothing resumes" is a
		// property of every client, enforced only by a guard test in client/, and
		// one latency-motivated cache away from being false. Refusing to issue
		// tickets makes the pin unbypassable from THIS end regardless of what any
		// client does later, which is the end we control.
		SessionTicketsDisabled: true,
	}, nil
}

// admitClientCertificate is the listener's VerifyPeerCertificate: the policy that
// replaces the chain verification RequestClientCert does not perform.
//
// # IT ADMITS. IT DOES NOT AUTHORISE. Do not read a nil return as approval.
//
// This is the sentence to keep hold of, because a callback whose success case is
// `return nil` is the exact shape of the silent-accept bug this repo has written
// warnings about in client/pin.go. The difference is that on the CLIENT side the
// pin IS the authorisation and the callback is where it lives; on the SERVER side
// there is nothing to pin AGAINST at handshake time, and there structurally
// cannot be:
//
//   - Enrolment MUST accept a certificate the bus has never seen. Accepting it is
//     how the binding gets made (MTLS-BIND) — the invite is what authorises it,
//     not the certificate. A handshake that refused unknown certificates would
//     make enrolment impossible.
//   - Every OTHER route wants a certificate already bound to an agent id, which
//     is a per-route decision over per-request state. crypto/tls has neither the
//     request nor the roster here.
//
// So the authorisation decision is deliberately deferred to the application layer
// (MTLS-BIND, MTLS-CROSSCHECK, and for peer buses RELAY-20), and what this
// function does is guarantee the ONE property all of them depend on: if a
// certificate reached the application, it is a certificate — a single leaf that
// crypto/x509 parses, from which a fingerprint can be derived. Anything else
// fails the handshake rather than arriving as a surprise nil somewhere
// downstream.
//
// # THE FINGERPRINT OF A PRESENTED CERTIFICATE HAS EXACTLY ONE SPELLING
//
// Written here because this is the file that puts the certificate on the
// connection, and every consumer downstream has to agree with every other one:
//
//	buscert.FingerprintOf(r.TLS.PeerCertificates[0])
//
// That is sha256 over the leaf's DER EXACTLY AS IT ARRIVED (x509.Certificate.Raw,
// never a re-marshalling of the parsed fields), rendered by String() as 64
// LOWERCASE hex characters with no prefix, no colons and no whitespace. It is the
// same construction the invite blob carries and client/pin.go pins the bus with,
// mirrored for the other direction. CALL THE HELPER; do not write a second one.
//
// The failure mode if a consumer computes its own is silent and expensive, which
// is why it is spelled out rather than left to be inferred: a digest over
// RawSubjectPublicKeyInfo instead of Raw, or base64 instead of hex, or uppercase,
// produces a value that is perfectly well-formed and NEVER MATCHES. Every peer
// connection is then refused as unknown, and nothing anywhere reports a hashing
// mismatch — it reads as a peering configuration fault.
//
// Do not name such a value `peerFingerprint`. That name is TAKEN, at
// internal/relay/peer.go, for the idempotency fingerprint of a roster payload —
// replay protection, with nothing to do with transport. A transport pin and a
// replay digest conflated by a future reader is a security bug, not a tidiness
// one.
//
// # TWO RULES FOR EVERY CONSUMER OF r.TLS.PeerCertificates (security gate)
//
// THE FINGERPRINT IS THE ONLY IDENTITY. Never Subject, CN, SAN, Issuer or
// SerialNumber: every one of those fields is chosen by whoever minted the
// certificate, which for a self-signed certificate is whoever presented it. They
// are attacker-controlled strings that happen to look like identity.
//
// CHECK THE SLICE IS NON-EMPTY FIRST, then INDEX [0] ONLY — NEVER ITERATE IT.
//
// Empty is not the exceptional case, it is the MAJORITY case: under
// RequestClientCert every ordinary agent connects without a certificate, so a
// consumer that reaches straight for PeerCertificates[0] panics on almost every
// connection. net/http recovers the panic per connection, so it presents as a
// mysterious dropped request rather than a crash.
//
// And never iterate: the peer controls the whole chain, while the handshake's
// CertificateVerify proves possession of the LEAF's private key and nothing else.
// A consumer that SEARCHED PeerCertificates for a known fingerprint would be
// spoofed by anyone who appended the victim's (public) certificate at index 1.
// Nothing does either of these today; they are written down so nothing starts.
//
// # WHAT IT MUST NOT GROW INTO
//
// A CertPool of enrolled agents' certificates, verified against. That is
// forbidden outright and the reason is not style: a pool entry is a TRUSTED ROOT,
// so any agent whose certificate was in it could mint certificates for any name
// and have them validate — every agent would be a CA for the whole bus. Identity
// here is a 32-byte fingerprint comparison, never chain building.
//
// An IsCA or ExtKeyUsage filter is also wrong here, and would break relay
// specifically. The bus's own certificate sets IsCA and carries BOTH ServerAuth
// and ClientAuth (internal/buscert), because a peer bus presents that same
// certificate when it dials another bus. Rejecting IsCA client certificates would
// refuse exactly the peer connection this task exists to make possible.
//
// # WHAT IT DOES NOT CHECK, and who owns the gap
//
// The VALIDITY WINDOW. RequestClientCert does no chain verification, so NotAfter
// is checked nowhere on this side of the connection: an expired agent certificate
// is admitted today. That is a known, filed gap (Spec Server
// ca356fde-0613-42cb-ac85-a629609d9c78), owned there rather than smuggled in
// here, and this comment exists so it stays a decision rather than an oversight.
//
// verifiedChains is named and ignored because it is ALWAYS nil in this
// configuration — crypto/tls only populates it at or above
// VerifyClientCertIfGiven, which this listener deliberately does not use. A
// future reader who assumed a chain were available would write a check that never
// runs.
func admitClientCertificate(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		// NO CLIENT CERTIFICATE. This is the ordinary path for every agent today
		// and it is a success, not a tolerated failure: the connection carries no
		// transport identity, which is precisely what the application layer will
		// observe as an empty r.TLS.PeerCertificates. Nothing is granted by
		// arriving here — the session token remains the credential.
		return nil
	}

	// Unreachable from a live handshake: crypto/tls parses every entry before it
	// calls this callback, so an empty or malformed leaf has already aborted the
	// handshake with a bad-certificate alert. Checked anyway, because a verifier
	// with a path that returns nil without having judged anything is how the
	// silent accept gets in, and because these two lines are the whole of the
	// promise the application layer is entitled to rely on.
	if len(rawCerts[0]) == 0 {
		return fmt.Errorf("the client presented an EMPTY leaf certificate; a presented certificate must be one from which a fingerprint can be derived")
	}
	if _, err := x509.ParseCertificate(rawCerts[0]); err != nil {
		return fmt.Errorf("the client's leaf certificate is not a parseable X.509 certificate: %w", err)
	}

	// ADMITTED — AND AUTHORISED FOR NOTHING. See this function's doc comment.
	return nil
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
