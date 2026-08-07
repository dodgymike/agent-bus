package main

// The ONE place cmd/agent-bus's tests learn how to talk to a bus (MTLS-LISTENER,
// invariant 11).
//
// Every test in this package that dials a server started by run() goes through
// busTestClient/busURL. Before MTLS-LISTENER those tests built a bare
// &http.Client{} and dialled http://; there is no plaintext listener any more,
// so all of them had to move at once, and they move THROUGH HERE rather than
// each growing its own tls.Config. A duplicated TLS configuration is how a
// single InsecureSkipVerify eventually gets added "just in this one test" --
// there is exactly one config to review, and TestCmdHasNoPlaintextListener
// guards the production half.
//
// # WHY RootCAs IS A REAL VERIFICATION AND NOT A LOOPHOLE
//
// The bus certificate is SELF-SIGNED and there is no CA anywhere in this design
// (DECISIONS.md E6). Go's verifier short-circuits when the leaf presented is
// itself in the root pool, so loading <data-dir>/bus-tls.crt as the SOLE root is
// a full x509 verification against exactly one certificate -- which also gets us
// the hostname check against the SANs and the validity-period check for free.
// That is strictly stronger than a fingerprint comparison and infinitely
// stronger than InsecureSkipVerify, which is BANNED in this package.
//
// The pool is built here with crypto/x509 directly rather than by calling
// busCertPool from cmd/agent-bus/healthcheck.go, deliberately: TestHealthcheckCommand
// asserts that busCertPool's verification is real, and a test client that
// borrowed the very function under test could only ever agree with it.

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/buscert"
)

// busTestTimeout bounds every request a test makes of a bus. Generous enough for
// a loaded CI box, short enough that a hung server fails the test rather than
// the suite's overall deadline.
const busTestTimeout = 10 * time.Second

// busTestClient returns an *http.Client that trusts EXACTLY the bus certificate
// written into dataDir, and nothing else -- not the system roots, and certainly
// not "whatever the server presents".
//
// It fails the test (rather than returning a client that cannot work) when the
// certificate is missing: a bus that started served TLS, so a missing
// bus-tls.crt means the test is pointed at the wrong data directory and every
// assertion after it would fail for the wrong reason.
func busTestClient(t *testing.T, dataDir string) *http.Client {
	t.Helper()

	certPath := filepath.Join(dataDir, buscert.CertFileName)
	pem := mustReadFile(t, certPath)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("%q holds no usable PEM CERTIFICATE block; a started bus always writes one", certPath)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			// The bus's own certificate is the only root. NO InsecureSkipVerify:
			// it is permitted in exactly one file in this repo (client/pin.go,
			// paired with VerifyPeerCertificate) and a test is never it.
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		},
	}
	t.Cleanup(tr.CloseIdleConnections)

	return &http.Client{Timeout: busTestTimeout, Transport: tr}
}

// busURL builds the https URL for path on a bus listening at addr.
//
// It is a function and not a `"https://"+addr+path` at each call site so that
// there is a single grep-able answer to "what scheme do the tests dial", and so
// a future change of scheme is one edit rather than twenty.
func busURL(addr, path string) string {
	return "https://" + addr + path
}
