package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/client"
	"github.com/dodgymike/agent-bus/internal/buscert"
)

// agentIDPattern is the shape a server-minted "testagent" enrolment must
// produce: "<bus-id>.<name>-<n>" (invariant 2), where the bus id is
// "bus-" + lowercase base32 (see internal/ids/busid.go).
var agentIDPattern = regexp.MustCompile(`^bus-[a-z0-9]+\.testagent-\d+$`)

// TestCLIEnrolEndToEnd is the load-bearing test for CLI-1/CLI-2: it builds
// and runs the REAL agent-bus server as a subprocess, then drives agent-busctl's
// in-process run() entry point against it exactly the way an agent would
// (enrol, whoami, whoami --verify, logout), asserting on the real wire
// protocol rather than a mock.
//
// It skips ONLY when the go toolchain itself is unavailable. A build failure
// or a runtime failure is a real failure and must not be swallowed by a skip.
func TestCLIEnrolEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available on PATH")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "agent-bus-test-server")
	buildCmd := exec.Command("go", "build", "-o", bin, "./cmd/agent-bus")
	buildCmd.Dir = repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("building ./cmd/agent-bus: %v\n%s", err, out)
	}

	// Grab a free loopback port ourselves: -listen 127.0.0.1:0 works (the
	// kernel picks a port), but nothing here can read back WHICH port the
	// subprocess bound without parsing its log output, and this is simpler
	// and just as reliable in practice. The small bind-close-rebind race is
	// accepted, per the brief.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("closing the port probe: %v", err)
	}

	dataDir := t.TempDir()
	serverCmd := exec.Command(bin, "-listen", addr, "-data-dir", dataDir, "-log-level", "error")
	// A LOCKED buffer, not a bare bytes.Buffer. os/exec copies the child's
	// stderr on its own goroutine whenever Stderr is not an *os.File, so every
	// read of it here races that writer. The race was real and had teeth: it
	// fires only when the server FAILS to start, which is precisely when the
	// diagnostic below is the only thing that explains the failure — so the
	// buffer destroyed the message it existed to print, exactly when it
	// mattered. Filed as 51710f76.
	serverStderr := &syncBuffer{}
	serverCmd.Stderr = serverStderr
	if err := serverCmd.Start(); err != nil {
		t.Fatalf("starting the agent-bus server: %v", err)
	}
	t.Cleanup(func() {
		if serverCmd.Process == nil {
			return
		}
		_ = serverCmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			_ = serverCmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = serverCmd.Process.Kill()
		}
	})

	// HTTPS, and pinned. The bus has served TLS ONLY since 9f2878a: there is no
	// plaintext listener to fall back to, so an http:// URL here does not
	// degrade, it simply never answers — which is exactly how this test spent
	// 21 seconds timing out instead of reporting anything useful.
	busURL := "https://" + addr
	busPool, busFingerprint := waitForBusCertificate(t, dataDir, serverStderr)
	waitForHealthz(t, busURL, busPool, serverStderr)

	identityDir := t.TempDir()
	ctx := context.Background()

	// enrol
	var enrolStdout, enrolStderr bytes.Buffer
	code := run(ctx, []string{"--identity", identityDir, "--bus", busURL, "--bus-fingerprint", busFingerprint,
		"enrol", "--name", "testagent", "--json"},
		&enrolStdout, &enrolStderr, emptyEnv)
	if code != client.ExitOK {
		t.Fatalf("enrol exit = %d, want 0; stdout=%q stderr=%q server_stderr=%s",
			code, enrolStdout.String(), enrolStderr.String(), serverStderr.String())
	}
	var enrolResult map[string]interface{}
	if err := json.Unmarshal(enrolStdout.Bytes(), &enrolResult); err != nil {
		t.Fatalf("enrol stdout is not JSON: %v (%q)", err, enrolStdout.String())
	}
	agentID, _ := enrolResult["agent_id"].(string)
	if agentID == "" {
		t.Fatalf("enrol result has no non-empty agent_id: %v", enrolResult)
	}
	if !agentIDPattern.MatchString(agentID) {
		t.Fatalf("agent_id = %q, want it to match %s", agentID, agentIDPattern.String())
	}

	// The enrolment above completed over a REAL TLS handshake with the REAL
	// bus, so by now the client must have minted its own certificate and
	// offered it (MTLS-CLIENTCERT). The bus does not ask for one yet — that is
	// MTLS-CLIENTAUTH — and this assertion is the proof that not-asking is
	// still a working enrolment rather than a broken one.
	clientKey := filepath.Join(identityDir, client.ClientTLSDirName, client.ClientKeyFileName)
	keyInfo, err := os.Stat(clientKey)
	if err != nil {
		t.Fatalf("the client did not mint its own TLS material during a real TLS enrolment: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("%s is mode %#o: readable or writable by other local users", clientKey, perm)
	}

	// And `client-cert` reports it without contacting the bus.
	var certStdout, certStderr bytes.Buffer
	if code := run(ctx, []string{"--identity", identityDir, "client-cert", "--json"}, &certStdout, &certStderr, emptyEnv); code != client.ExitOK {
		t.Fatalf("client-cert exit = %d, want 0; stdout=%q stderr=%q", code, certStdout.String(), certStderr.String())
	}
	var certResult map[string]interface{}
	if err := json.Unmarshal(certStdout.Bytes(), &certResult); err != nil {
		t.Fatalf("client-cert stdout is not JSON: %v (%q)", err, certStdout.String())
	}
	if fp, _ := certResult["fingerprint"].(string); len(fp) != 2*sha256.Size {
		t.Errorf("client-cert reported fingerprint %q, want %d hex characters", fp, 2*sha256.Size)
	}
	if created, _ := certResult["created"].(bool); created {
		t.Error("client-cert reported created=true, so it minted a SECOND certificate rather than reporting the one enrolment already used")
	}

	// whoami
	var whoamiStdout, whoamiStderr bytes.Buffer
	code = run(ctx, []string{"--identity", identityDir, "whoami", "--json"}, &whoamiStdout, &whoamiStderr, emptyEnv)
	if code != client.ExitOK {
		t.Fatalf("whoami exit = %d, want 0; stdout=%q stderr=%q", code, whoamiStdout.String(), whoamiStderr.String())
	}
	var whoamiResult map[string]interface{}
	if err := json.Unmarshal(whoamiStdout.Bytes(), &whoamiResult); err != nil {
		t.Fatalf("whoami stdout is not JSON: %v (%q)", err, whoamiStdout.String())
	}
	if got, _ := whoamiResult["agent_id"].(string); got != agentID {
		t.Fatalf("whoami agent_id = %q, want %q (the id enrol reported)", got, agentID)
	}

	// whoami --verify: a real session handshake against the real server.
	var verifyStdout, verifyStderr bytes.Buffer
	code = run(ctx, []string{"--identity", identityDir, "whoami", "--verify", "--json"}, &verifyStdout, &verifyStderr, emptyEnv)
	if code != client.ExitOK {
		t.Fatalf("whoami --verify exit = %d, want 0; stdout=%q stderr=%q", code, verifyStdout.String(), verifyStderr.String())
	}
	var verifyResult map[string]interface{}
	if err := json.Unmarshal(verifyStdout.Bytes(), &verifyResult); err != nil {
		t.Fatalf("whoami --verify stdout is not JSON: %v (%q)", err, verifyStdout.String())
	}
	session, ok := verifyResult["session"].(map[string]interface{})
	if !ok || session == nil {
		t.Fatalf("whoami --verify result has no session object: %v", verifyResult)
	}
	if expiresAt, _ := session["expires_at"].(string); expiresAt == "" {
		t.Fatalf("session has no expires_at: %v", session)
	}

	// logout
	var logoutStdout, logoutStderr bytes.Buffer
	code = run(ctx, []string{"--identity", identityDir, "logout", "--json"}, &logoutStdout, &logoutStderr, emptyEnv)
	if code != client.ExitOK {
		t.Fatalf("logout exit = %d, want 0; stdout=%q stderr=%q", code, logoutStdout.String(), logoutStderr.String())
	}
	var logoutResult map[string]interface{}
	if err := json.Unmarshal(logoutStdout.Bytes(), &logoutResult); err != nil {
		t.Fatalf("logout stdout is not JSON: %v (%q)", err, logoutStdout.String())
	}
	notified, ok := logoutResult["server_notified"].(bool)
	if !ok || notified {
		t.Fatalf("logout server_notified = %v, want false (the bus has no /v1/leave route yet)", logoutResult["server_notified"])
	}
}

// syncBuffer is a bytes.Buffer that may be written by os/exec's copier
// goroutine while the test reads it.
//
// It exists because the obvious spelling — handing a bare *bytes.Buffer to
// cmd.Stderr and calling String() on failure — is a data race, and one that
// only fires on the failure path. Under -race that turns "the server did not
// start, here is why" into "race detected", losing the diagnostic at the only
// moment anyone wanted it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForBusCertificate waits for the bus to mint its self-signed TLS material
// and returns a pool trusting it, plus its fingerprint in the spelling
// --bus-fingerprint expects.
//
// The certificate is read FROM THE FILE the bus wrote, not scraped from its
// log. Log-scraping is how a local attacker who can pre-create a file gets to
// choose which certificate a client trusts, and a security gate found exactly
// that pattern elsewhere today; there is no reason to add a second instance of
// it in a test.
//
// It is polled rather than assumed present because the file appears during
// startup, concurrently with this test.
func waitForBusCertificate(t *testing.T, dataDir string, serverStderr *syncBuffer) (*x509.CertPool, string) {
	t.Helper()
	certPath := filepath.Join(dataDir, buscert.CertFileName)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pemBytes, err := os.ReadFile(certPath)
		if err == nil {
			if block, _ := pem.Decode(pemBytes); block != nil && block.Type == "CERTIFICATE" {
				leaf, perr := x509.ParseCertificate(block.Bytes)
				if perr != nil {
					t.Fatalf("the bus wrote %s but it does not parse: %v", certPath, perr)
				}
				pool := x509.NewCertPool()
				pool.AddCert(leaf)
				sum := sha256.Sum256(leaf.Raw)
				return pool, hex.EncodeToString(sum[:])
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the bus never wrote %s; server stderr:\n%s", certPath, serverStderr.String())
	return nil, ""
}

// waitForHealthz polls GET /healthz until it answers 200, with a bounded
// timeout, failing the test rather than hanging the suite if the server never
// comes up.
//
// Verification is FULL and standard — the bus certificate is a trusted root in
// busPool and its subject alternative names cover the loopback address this
// dials. Nothing is relaxed to make this work, and nothing here may relax it:
// client/guard_test.go scans this directory too, and the one file permitted to
// disable the default check is client/pin.go.
func waitForHealthz(t *testing.T, busURL string, busPool *x509.CertPool, serverStderr *syncBuffer) {
	t.Helper()
	hc := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: busPool, MinVersion: tls.VersionTLS12},
		},
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := hc.Get(busURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server at %s never answered /healthz within the deadline; server stderr:\n%s", busURL, serverStderr.String())
}
