package main

// `agent-bus healthcheck` — the operator/deployment liveness probe for a
// TLS-ONLY bus (MTLS-VERIFY).
//
// # Why this exists at all
//
// MTLS-LISTENER makes the bus serve https and nothing else. That breaks every
// plaintext probe pointed at it, and the probes are not optional decoration:
// Dockerfile's HEALTHCHECK and docker-compose.yml's healthcheck are what
// `docker compose ps` reports "healthy" from and what a future
// `depends_on: condition: service_healthy` would gate on. A container whose
// probe can never succeed is a container Docker restarts forever.
//
// The runtime image is Alpine with busybox `wget` and no curl (see Dockerfile,
// which chose Alpine precisely so a probe would not need a second binary). And
// busybox `wget` cannot be told to trust ONE self-signed certificate: its only
// relevant knob is --no-check-certificate, which does not verify a different
// way, it verifies not at all. Invariant 11 is explicit that certificate
// verification is never disabled to make something work, and never behind a flag
// that does it silently — so the probe moved into the binary that already ships
// in the image rather than the certificate check being dropped to keep the probe.
//
// # It is a SUBCOMMAND ON THE SERVER BINARY, following `invite mint`
//
// Same reason (DECISIONS.md E4, INVITE-MINT): its input is FILESYSTEM ACCESS to
// the data directory, not a network privilege. Nothing new is exposed on the
// wire. It differs from `invite mint` in one respect that matters — it takes NO
// LOCK and writes NOTHING, so unlike minting it is safe, and expected, to run
// against a RUNNING bus holding the dirlock. It opens exactly one file, the
// world-readable certificate.
//
// It is deliberately NOT part of `agent-busctl`, the agent-facing client
// (invariant 7). An agent never runs this: it needs no session, no enrolment and
// no identity, and it reads the bus's data directory, which no agent has. It is
// the operator's answer to "is this process serving", and it is in the image the
// operator deploys.
//
// # WHAT IT ACTUALLY CHECKS, which is more than "did something answer"
//
// It trusts EXACTLY ONE certificate: the PEM in the data directory, loaded as
// the sole root. That is not a weaker check than a public-CA chain, it is a
// narrower one, and the narrowness is the point — the only certificate this
// probe accepts is the one this data directory holds. Because it is a real
// x509 verification rather than a pin, it also enforces, for free:
//
//   - the HOSTNAME, against the certificate's SANs; and
//   - the VALIDITY PERIOD. An expired bus certificate fails the healthcheck and
//     the container is reported unhealthy. DECISIONS.md chose a 365-day lifetime
//     as a leak-containment bound, and a probe that ignored expiry would let a
//     bus no client can dial keep reporting itself healthy indefinitely.
//
// There is no InsecureSkipVerify here and none may be added: it is permitted in
// exactly one file in this repo (client/pin.go, paired with
// VerifyPeerCertificate), and this file is not it.

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dodgymike/agent-bus/internal/buscert"
)

// healthcheckCommandName is the single word main() intercepts to reach this
// command.
const healthcheckCommandName = "healthcheck"

// healthcheckPath is the unauthenticated liveness route (internal/httpapi).
// Unauthenticated is what makes a probe possible at all: a healthcheck holding a
// session credential would be a credential in a container's HEALTHCHECK line.
const healthcheckPath = "/healthz"

// defaultHealthcheckTimeout bounds the whole probe — connect, handshake, request
// and response. It is well inside the 3s `timeout:` both healthcheck definitions
// declare, so the probe reports a failure rather than being killed mid-flight
// with no message.
const defaultHealthcheckTimeout = 2 * time.Second

// Exit codes. They are a contract (CONTRACTS-CLI.md) because a HEALTHCHECK line
// and a shell wrapper both branch on them.
const (
	// healthcheckOK: the bus answered 200 over a verified TLS connection.
	healthcheckOK = 0
	// healthcheckUnhealthy: it did not. Unreachable, an unusable or untrusted
	// certificate, a TLS failure, a timeout, or a non-200 status all land here.
	// They are ONE code on purpose: `docker inspect` shows the message, and a
	// probe with a taxonomy of failure codes invites a HEALTHCHECK that treats
	// some of them as healthy.
	healthcheckUnhealthy = 1
	// healthcheckUsage: bad flags. Distinct from unhealthy because a typo in a
	// HEALTHCHECK line otherwise looks exactly like a dead bus.
	healthcheckUsage = 2
)

// runHealthcheckCommand implements `agent-bus healthcheck`. It returns the
// process exit code and never calls os.Exit, so it stays testable.
func runHealthcheckCommand(args []string, stdout, stderr io.Writer) int {
	var (
		dataDir string
		addr    string
		timeout time.Duration
	)

	fs := flag.NewFlagSet("agent-bus "+healthcheckCommandName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage of agent-bus %s:\n", healthcheckCommandName)
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nProbes GET https://<addr>%s, trusting ONLY the certificate in <data-dir>/%s.\n"+
			"Exit %d healthy, %d unhealthy, %d bad usage.\n",
			healthcheckPath, buscert.CertFileName, healthcheckOK, healthcheckUnhealthy, healthcheckUsage)
	}
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "the bus's data directory; only "+buscert.CertFileName+" is read from it, and no lock is taken")
	fs.StringVar(&addr, "addr", defaultListen, "host:port to probe; must match the bus's -listen")
	fs.DurationVar(&timeout, "timeout", defaultHealthcheckTimeout, "bound on the whole probe: connect, TLS handshake, request and response")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h is a successful invocation of the help, not an unhealthy bus.
			return healthcheckOK
		}
		return healthcheckUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agent-bus %s: unexpected argument %q\n", healthcheckCommandName, fs.Arg(0))
		return healthcheckUsage
	}
	if dataDir == "" {
		fmt.Fprintf(stderr, "agent-bus %s: -data-dir must not be empty\n", healthcheckCommandName)
		return healthcheckUsage
	}
	if timeout <= 0 {
		fmt.Fprintf(stderr, "agent-bus %s: -timeout must be positive, got %s\n", healthcheckCommandName, timeout)
		return healthcheckUsage
	}

	url, err := healthcheckURL(addr)
	if err != nil {
		fmt.Fprintf(stderr, "agent-bus %s: %v\n", healthcheckCommandName, err)
		return healthcheckUsage
	}

	certPath := filepath.Join(dataDir, buscert.CertFileName)
	roots, err := busCertPool(certPath)
	if err != nil {
		fmt.Fprintf(stderr, "agent-bus %s: %v\n", healthcheckCommandName, err)
		return healthcheckUnhealthy
	}

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				// The bus's own self-signed certificate is the ONLY root. Go's
				// verifier short-circuits a self-signed certificate that is
				// itself in the root pool, so this both chains and pins.
				RootCAs:    roots,
				MinVersion: tls.VersionTLS12,
			},
			// No connection reuse to bank: this process makes one request and
			// exits. Disabling keep-alives means the probe never leaves a
			// half-open connection behind on a bus it just declared unhealthy.
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(stderr, "agent-bus %s: %s is not healthy: %v\n", healthcheckCommandName, url, err)
		fmt.Fprintf(stderr, "  the bus serves TLS ONLY (invariant 11): a plaintext probe, or one trusting a different certificate than %s, will always fail here.\n", certPath)
		return healthcheckUnhealthy
	}
	defer resp.Body.Close()
	// Drained rather than ignored so the server sees a completed response.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "agent-bus %s: %s answered %d, want %d\n", healthcheckCommandName, url, resp.StatusCode, http.StatusOK)
		return healthcheckUnhealthy
	}
	fmt.Fprintf(stdout, "ok %s\n", url)
	return healthcheckOK
}

// healthcheckURL turns a host:port into the https URL to probe.
//
// A WILDCARD BIND is rewritten to loopback. ":8080", "0.0.0.0:8080" and
// "[::]:8080" are legal -listen values but are not addresses anything dials, and
// the certificate does not name them either — internal/buscert deliberately
// drops the wildcard from the SANs (see certHosts). Probing loopback instead is
// correct for both healthchecks: each runs INSIDE the container's network
// namespace, so loopback is the bus.
func healthcheckURL(addr string) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("-addr must not be empty")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid -addr %q: %w", addr, err)
	}
	if port == "" {
		return "", fmt.Errorf("invalid -addr %q: no port", addr)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "https://" + net.JoinHostPort(host, port) + healthcheckPath, nil
}

// busCertPool loads the bus certificate as the sole trusted root.
//
// maxBusCertFileSize bounds the read: this path is reached before anything is
// verified, and a probe that tries to load a multi-gigabyte "certificate" into a
// container's memory is a way to make a healthy bus report unhealthy.
func busCertPool(certPath string) (*x509.CertPool, error) {
	f, err := os.Open(certPath)
	if err != nil {
		return nil, fmt.Errorf("reading the bus certificate %q: %w\n  it is written by the bus on its first start; a healthcheck must run against the bus's own -data-dir", certPath, err)
	}
	defer f.Close()

	pem, err := io.ReadAll(io.LimitReader(f, maxBusCertFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading the bus certificate %q: %w", certPath, err)
	}
	if int64(len(pem)) > maxBusCertFileSize {
		return nil, fmt.Errorf("the bus certificate %q is larger than %d bytes; refusing to load it", certPath, maxBusCertFileSize)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("the bus certificate %q holds no usable PEM CERTIFICATE block", certPath)
	}
	return pool, nil
}

// maxBusCertFileSize is a generous ceiling on a single PEM certificate; the real
// file is well under 1 KiB.
const maxBusCertFileSize int64 = 64 << 10
