package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/client"
)

// agentIDPattern is the shape a server-minted "testagent" enrolment must
// produce: "<bus-id>.<name>-<n>" (invariant 2), where the bus id is
// "bus-" + lowercase base32 (see internal/ids/busid.go).
var agentIDPattern = regexp.MustCompile(`^bus-[a-z0-9]+\.testagent-\d+$`)

// TestCLIEnrolEndToEnd is the load-bearing test for CLI-1/CLI-2: it builds
// and runs the REAL agent-bus server as a subprocess, then drives busctl's
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
	var serverStderr bytes.Buffer
	serverCmd.Stderr = &serverStderr
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

	busURL := "http://" + addr
	waitForHealthz(t, busURL, &serverStderr)

	identityDir := t.TempDir()
	ctx := context.Background()

	// enrol
	var enrolStdout, enrolStderr bytes.Buffer
	code := run(ctx, []string{"--identity", identityDir, "--bus", busURL, "enrol", "--name", "testagent", "--json"},
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

// waitForHealthz polls GET /healthz until it answers 200, with a bounded
// timeout, failing the test rather than hanging the suite if the server never
// comes up.
func waitForHealthz(t *testing.T, busURL string, serverStderr *bytes.Buffer) {
	t.Helper()
	hc := &http.Client{Timeout: 2 * time.Second}
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
