package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/client"
)

// TestClientCertWarningReachesStderr covers the DRAIN, not the detection.
//
// The detection already existed and was already tested: a private key found
// readable by other local users is chmodded back to 0600 and a warning is
// recorded. What did not exist was anybody reading that warning. It was
// surfaced only by `agent-busctl client-cert` — a command whose own help says
// you rarely need to run it — so on every ordinary command the key was
// silently tightened and the operator never told, which client/clientcert.go
// itself argues is the actual defect ("tightening without saying so is the part
// that would be wrong").
//
// The assertion is deliberately on STDERR and on the exit code together: the
// warning must not become a failure, and it must not land on stdout, where it
// would corrupt a --json consumer's document.
func TestClientCertWarningReachesStderr(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: 0644 is not 'readable by others' in any meaningful sense here")
	}
	dir := t.TempDir()

	// Mint, then loosen the key the way a bad umask or a careless copy would.
	cc, err := client.LoadOrCreateClientCertificate(dir)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if err := os.Chmod(cc.KeyPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	res := runCLI(t, emptyEnv, "--identity", dir, "client-cert", "--json")
	if res.Code != client.ExitOK {
		t.Fatalf("client-cert exit = %d, want 0; a loose key is a warning, not a failure — refusing to run would not make an already-exposed key any less exposed. stderr=%q", res.Code, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "WARNING") || !strings.Contains(res.Stderr, "readable by other local users") {
		t.Errorf("stderr = %q, want a WARNING naming the exposure", res.Stderr)
	}
	// EXACTLY ONCE. `Contains` alone let a duplicate through — the security
	// gate found the warning printed twice, because this command drained it
	// itself AND run() drained it afterwards. A warning repeated is a warning
	// an operator learns to skim.
	if n := strings.Count(res.Stderr, "readable by other local users"); n != 1 {
		t.Errorf("the warning appears %d times in stderr, want exactly 1:\n%s", n, res.Stderr)
	}
	if strings.Contains(res.Stdout, "WARNING") {
		t.Errorf("the warning landed on STDOUT (%q), which breaks a --json consumer's document", res.Stdout)
	}
	// And it was actually repaired, not merely reported.
	fi, err := os.Stat(cc.KeyPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("%s is still mode %#o", cc.KeyPath, perm)
	}
}

// TestClientCertCommandIsIdempotent asserts the subcommand does not mint a
// second certificate, which would silently invalidate any binding the bus held.
func TestClientCertCommandIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	first := runCLI(t, emptyEnv, "--identity", dir, "client-cert", "--json")
	if first.Code != client.ExitOK {
		t.Fatalf("first run exit = %d; stderr=%q", first.Code, first.Stderr)
	}
	if !strings.Contains(first.Stdout, `"created":true`) {
		t.Errorf("first run did not report created=true: %s", first.Stdout)
	}
	keyBefore, err := os.ReadFile(filepath.Join(dir, client.ClientTLSDirName, client.ClientKeyFileName))
	if err != nil {
		t.Fatalf("reading the key: %v", err)
	}

	second := runCLI(t, emptyEnv, "--identity", dir, "client-cert", "--json")
	if second.Code != client.ExitOK {
		t.Fatalf("second run exit = %d; stderr=%q", second.Code, second.Stderr)
	}
	if !strings.Contains(second.Stdout, `"created":false`) {
		t.Errorf("second run did not report created=false: %s", second.Stdout)
	}
	keyAfter, err := os.ReadFile(filepath.Join(dir, client.ClientTLSDirName, client.ClientKeyFileName))
	if err != nil {
		t.Fatalf("re-reading the key: %v", err)
	}
	if string(keyBefore) != string(keyAfter) {
		t.Error("the private key was rewritten by a second `client-cert` run")
	}
}
