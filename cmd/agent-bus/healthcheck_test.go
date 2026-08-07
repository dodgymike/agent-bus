package main

// Tests for `agent-bus healthcheck` (MTLS-VERIFY): the probe Dockerfile's
// HEALTHCHECK and docker-compose.yml's healthcheck run inside the container.
//
// # The assertion that carries the weight
//
// It is NOT "the probe says ok when the bus is up" -- a probe that skipped
// verification would pass that too, and busybox `wget --no-check-certificate`
// (the thing this subcommand exists to replace) would pass it as well. The
// load-bearing case is "WRONG CERTIFICATE, WRONG ANSWER": pointed at a data
// directory whose certificate is a different one, against a bus that is up and
// perfectly healthy, the probe must report UNHEALTHY. That is the only case that
// can tell a real x509 verification apart from an InsecureSkipVerify, and
// invariant 11 is explicit that verification is never disabled to make a probe
// work.
//
// Exit codes are contract (CONTRACTS-CLI.md): 0 healthy, 1 unhealthy, 2 usage.
// The 1/2 split matters operationally -- a typo in a HEALTHCHECK line otherwise
// looks exactly like a dead bus -- so the table below pins both.

import (
	"bytes"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dodgymike/agent-bus/internal/buscert"
)

// runHealthcheck invokes the subcommand and returns its exit code plus both
// streams, mirroring runMint in invite_test.go.
func runHealthcheck(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := runHealthcheckCommand(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestHealthcheckCommand(t *testing.T) {
	// A real bus, over the real TLS listener. Nothing here is stubbed: the
	// probe's whole subject is a live handshake.
	dir := t.TempDir()
	proc := startServer(t, dir)
	addr := proc.awaitServerStarted(t)

	// NOT VACUOUS: the bus is genuinely serving before a single probe runs, so
	// an "unhealthy" verdict below is about the probe's input and never about a
	// process that failed to come up.
	mustGetHealthz(t, dir, addr)

	// A data directory holding NO certificate at all.
	noCertDir := t.TempDir()
	// A data directory holding a VALID but DIFFERENT certificate -- a real bus
	// identity, minted the same way, simply not this bus's.
	otherDir, _, _ := initDataDir(t)
	// An address nothing listens on.
	deadAddr := freeLoopbackAddr(t)
	mustBeUnbound(t, deadAddr, "before the not-running probe")

	healthyURL := "https://" + addr + healthcheckPath

	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout []string
		wantStderr []string
		why        string
	}{
		{
			name:       "a healthy bus",
			args:       []string{"-data-dir", dir, "-addr", addr},
			wantCode:   healthcheckOK,
			wantStdout: []string{"ok ", healthyURL},
			why:        "the probe must name the URL it verified, so `docker inspect` shows what was actually checked",
		},
		{
			name:     "an explicit timeout is honoured on the happy path",
			args:     []string{"-data-dir", dir, "-addr", addr, "-timeout", "5s"},
			wantCode: healthcheckOK,
			why:      "the HEALTHCHECK lines pass a timeout; it must not change the verdict",
		},
		{
			name:       "a data dir with no certificate",
			args:       []string{"-data-dir", noCertDir, "-addr", addr},
			wantCode:   healthcheckUnhealthy,
			wantStderr: []string{filepath.Join(noCertDir, buscert.CertFileName), "data-dir"},
			why:        "the refusal must name the missing file: the usual cause is a probe pointed at the wrong -data-dir, and the path is the whole diagnosis",
		},
		{
			// THE ASSERTION THAT PROVES VERIFICATION IS REAL. The bus is up and
			// healthy; only the trust anchor is wrong. An InsecureSkipVerify
			// probe, or a probe that merely opened a socket, would say "ok".
			name:       "a healthy bus presenting a DIFFERENT certificate",
			args:       []string{"-data-dir", otherDir, "-addr", addr},
			wantCode:   healthcheckUnhealthy,
			wantStderr: []string{"is not healthy", "x509", filepath.Join(otherDir, buscert.CertFileName)},
			why:        "a probe that accepted any certificate would report a bus it cannot authenticate as healthy; that is the exact failure invariant 11 forbids",
		},
		{
			name:       "the bus is not running",
			args:       []string{"-data-dir", dir, "-addr", deadAddr},
			wantCode:   healthcheckUnhealthy,
			wantStderr: []string{"is not healthy", deadAddr},
			why:        "nothing is listening; unhealthy, not a usage error",
		},
		{
			name:       "a zero timeout",
			args:       []string{"-data-dir", dir, "-addr", addr, "-timeout", "0"},
			wantCode:   healthcheckUsage,
			wantStderr: []string{"-timeout must be positive"},
			why:        "a zero timeout is a typo in a HEALTHCHECK line, and must not be reported as a dead bus",
		},
		{
			name:       "a negative timeout",
			args:       []string{"-data-dir", dir, "-addr", addr, "-timeout", "-1s"},
			wantCode:   healthcheckUsage,
			wantStderr: []string{"-timeout must be positive"},
			why:        "same as above; the sign is not a special case",
		},
		{
			name:       "an unexpected positional argument",
			args:       []string{"-data-dir", dir, "-addr", addr, "now"},
			wantCode:   healthcheckUsage,
			wantStderr: []string{`unexpected argument "now"`},
			why:        "a stray word is a typo, not a verdict about the bus",
		},
		{
			name:       "an empty data dir flag",
			args:       []string{"-data-dir", "", "-addr", addr},
			wantCode:   healthcheckUsage,
			wantStderr: []string{"-data-dir must not be empty"},
			why:        "an unset environment variable expands to empty; that is a usage error, not an unhealthy bus",
		},
		{
			name:       "a malformed address",
			args:       []string{"-data-dir", dir, "-addr", "not-an-address"},
			wantCode:   healthcheckUsage,
			wantStderr: []string{"invalid -addr"},
			why:        "an address with no port can never be probed; fail as usage rather than as unhealthy",
		},
		{
			name:       "an unknown flag",
			args:       []string{"-data-dir", dir, "-nope"},
			wantCode:   healthcheckUsage,
			wantStderr: []string{"flag provided but not defined"},
			why:        "flag.ContinueOnError must map to the usage code, not to unhealthy",
		},
		{
			name:       "-h",
			args:       []string{"-h"},
			wantCode:   healthcheckOK,
			wantStderr: []string{healthcheckPath, buscert.CertFileName},
			why:        "asking for help is a successful invocation of the help, and the help must say what is verified",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runHealthcheck(tc.args...)
			if code != tc.wantCode {
				t.Fatalf("runHealthcheckCommand(%q) = %d, want %d: %s\nstdout: %s\nstderr: %s", tc.args, code, tc.wantCode, tc.why, stdout, stderr)
			}
			for _, want := range tc.wantStdout {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout %q does not contain %q: %s", stdout, want, tc.why)
				}
			}
			for _, want := range tc.wantStderr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not contain %q: %s", stderr, want, tc.why)
				}
			}
			// A failing probe must never print an "ok" line: a HEALTHCHECK that
			// grepped stdout would otherwise be told the opposite of the truth.
			if tc.wantCode != healthcheckOK && strings.Contains(stdout, "ok ") {
				t.Errorf("a probe that exited %d still printed an ok line on stdout: %q", code, stdout)
			}
			// And it must never leak the private key material it sits next to.
			for _, out := range []string{stdout, stderr} {
				if strings.Contains(out, "PRIVATE KEY") || strings.Contains(out, "BEGIN ") {
					t.Errorf("the probe printed something that looks like PEM key material: %q", out)
				}
			}
		})
	}

	proc.signal(t, syscall.SIGTERM)
	if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
	}
}

// TestHealthcheckURL is the unit table for the one piece of judgement in
// healthcheck.go: turning a -listen value into an address something can dial.
//
// The wildcard rewrite is not cosmetic. ":8080", "0.0.0.0:8080" and "[::]:8080"
// are all legal -listen values, none of them is an address anything dials, and
// internal/buscert deliberately keeps them OUT of the certificate's SANs (see
// certHosts). Probing the literal value would fail hostname verification even
// against a perfectly healthy bus. Loopback is right for both healthchecks
// because each runs inside the container's own network namespace.
func TestHealthcheckURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		addr    string
		want    string
		wantErr string // substring; empty means "must succeed"
		why     string
	}{
		{
			name: "explicit loopback",
			addr: "127.0.0.1:8080",
			want: "https://127.0.0.1:8080/healthz",
			why:  "the default -listen, dialled exactly as given",
		},
		{
			name: "wildcard bind, empty host",
			addr: ":8080",
			want: "https://127.0.0.1:8080/healthz",
			why:  `"https://:8080/healthz" is not dialable, and the certificate does not name the wildcard`,
		},
		{
			name: "wildcard bind, 0.0.0.0",
			addr: "0.0.0.0:8080",
			want: "https://127.0.0.1:8080/healthz",
			why:  "0.0.0.0 is a bind address, never a destination",
		},
		{
			name: "wildcard bind, ::",
			addr: "[::]:8080",
			want: "https://127.0.0.1:8080/healthz",
			why:  ":: is the IPv6 wildcard; loopback reaches a bus bound to it",
		},
		{
			name: "a hostname is left alone",
			addr: "localhost:8080",
			want: "https://localhost:8080/healthz",
			why:  "a name is dialable and IS in the certificate's SANs; rewriting it would break hostname verification",
		},
		{
			name: "an IPv6 literal keeps its brackets",
			addr: "[::1]:8080",
			want: "https://[::1]:8080/healthz",
			why:  "::1 is loopback already, and the URL form needs the brackets back",
		},
		{
			name:    "no port",
			addr:    "not-an-address",
			wantErr: "invalid -addr",
			why:     "SplitHostPort refuses it, and a probe cannot invent a port",
		},
		{
			name:    "empty",
			addr:    "",
			wantErr: "must not be empty",
			why:     "an unset environment variable expands to this",
		},
		{
			name:    "a host with an empty port",
			addr:    "127.0.0.1:",
			wantErr: "no port",
			why:     "SplitHostPort accepts it, so the emptiness has to be caught explicitly or the URL becomes https://127.0.0.1:/healthz",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := healthcheckURL(tc.addr)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("healthcheckURL(%q) = %q, want an error containing %q: %s", tc.addr, got, tc.wantErr, tc.why)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("healthcheckURL(%q) error = %q, want it to contain %q", tc.addr, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("healthcheckURL(%q) unexpected error: %v", tc.addr, err)
			}
			if got != tc.want {
				t.Fatalf("healthcheckURL(%q) = %q, want %q: %s", tc.addr, got, tc.want, tc.why)
			}
		})
	}
}
