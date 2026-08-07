package main

// The MTLS-VERIFY LIVE PROOF: `scripts/bus-serve.sh` must bring up a TLS-only
// bus and report it healthy, exercised THE WAY AN AGENT WOULD.
//
// # Why this shells out instead of asserting the same thing in Go
//
// CLAUDE.md: "For anything agent-facing, ALSO exercise it the way an agent
// would: through scripts/bus-*.sh against a running server, not through a
// hand-written curl. If the wrapper doesn't work, the feature doesn't work."
//
// That is not ceremony here, it is the actual risk. MTLS-LISTENER makes the
// server https-only, which BREAKS every plaintext probe pointed at it -- and the
// wrapper's `start` does not return until its probe succeeds. So a wrapper left
// on http:// would report a perfectly healthy bus as a failed start, and every
// other task's server-startup step in this repo would break with it. The two
// halves had to move in one commit, and this test is the thing that fails if
// only one of them did. A Go test that re-implemented the probe would pass over
// exactly that defect, because it would not be running the code an agent runs.
//
// # Nothing here touches the tracked data/ dir
//
// AGENT_BUS_RUN_DIR and AGENT_BUS_DATA_DIR are pointed at fresh subdirectories
// of t.TempDir() (which is under /tmp), and AGENT_BUS_LISTEN at an ephemeral
// loopback port this test reserved and released. The wrapper's own defaults
// (/tmp/agent-bus, 127.0.0.1:8080) are deliberately NOT used: a developer's real
// bus may be running on them, and a test that stopped it would be worse than a
// test that failed.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// busServeTimeout bounds each wrapper invocation. `start` builds the binary
// first, so it is generous; a hung script still fails the test rather than the
// suite.
const busServeTimeout = 3 * time.Minute

// The wrapper's documented exit codes (CONTRACTS-AGENT.md), asserted by name so
// a failure message says which contract broke.
const (
	busServeOK         = 0 // start: started and healthy / status: running and healthy / stop: stopped
	busServeNotRunning = 3 // status: no pidfile, or a stale one
)

func TestLiveBusServeWrapperOverTLS(t *testing.T) {
	// SKIP ONLY FOR A GENUINELY ABSENT TOOL. The wrapper is bash and its probe is
	// curl; neither being installed is an environment fact, not a result. Any
	// other failure below is a real failure and must not be skipped past.
	for _, tool := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: scripts/bus-serve.sh cannot run here (%v)", tool, err)
		}
	}

	// The test's working directory is the package directory, cmd/agent-bus.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the repo root from cmd/agent-bus: %v", err)
	}
	script := filepath.Join(repoRoot, "scripts", "bus-serve.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("stat %q: %v; this test exercises the wrapper an agent uses, and cannot substitute for it", script, err)
	}

	base := t.TempDir()
	runDir := filepath.Join(base, "run")
	dataDir := filepath.Join(base, "data")
	listen := freeLoopbackAddr(t)
	mustBeUnbound(t, listen, "before bus-serve.sh start")

	env := append(os.Environ(),
		"AGENT_BUS_RUN_DIR="+runDir,
		"AGENT_BUS_DATA_DIR="+dataDir,
		"AGENT_BUS_LISTEN="+listen,
		"AGENT_BUS_LOG_LEVEL=info",
	)

	busServe := func(t *testing.T, sub string) (int, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), busServeTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", script, sub)
		cmd.Dir = repoRoot
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("scripts/bus-serve.sh %s did not finish within %s\n%s", sub, busServeTimeout, out)
		}
		code := 0
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("running %s %s: %v\n%s", script, sub, err, out)
			}
			code = ee.ExitCode()
		}
		t.Logf("bus-serve.sh %s -> exit %d\n%s", sub, code, out)
		return code, string(out)
	}

	// However this test ends -- pass, fail, panic or timeout -- the bus stops.
	// Registered BEFORE the first start so an assertion failure inside start
	// cannot leave a process behind holding a port and a data directory.
	t.Cleanup(func() {
		cmd := exec.Command("bash", script, "stop")
		cmd.Dir = repoRoot
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("cleanup: bus-serve.sh stop: %v\n%s", err, out)
		}
	})

	// --- start: THE assertion of MTLS-VERIFY ---
	//
	// cmd_start polls its own health probe and only returns 0 once /healthz has
	// answered. Since MTLS-LISTENER that probe must be
	// `curl --cacert <data-dir>/bus-tls.crt https://...`; an http:// probe, or a
	// missing --cacert, cannot succeed against this bus and this exits 1.
	if code, out := busServe(t, "start"); code != busServeOK {
		t.Fatalf("scripts/bus-serve.sh start = %d, want %d. The bus serves TLS ONLY (invariant 11), so the wrapper's health probe must be an https request verifying %s. A non-zero exit here means the wrapper and the listener disagree about the scheme -- which breaks every task in this repo that starts a server.\n%s",
			code, busServeOK, filepath.Join(dataDir, "bus-tls.crt"), out)
	} else {
		// The operator-facing output has to name the trust anchor, because there
		// is no trust-on-first-use: an agent cannot enrol without the
		// fingerprint, and it must not have to go and read the log for it.
		for _, want := range []string{"https ONLY", "fingerprint ", "certificate "} {
			if !strings.Contains(out, want) {
				t.Errorf("`bus-serve.sh start` output does not mention %q; without it an agent has nothing to pass to --bus-fingerprint and there is deliberately no TOFU\n%s", want, out)
			}
		}
	}

	// The wrapper really did write the certificate into OUR data dir, not the
	// default one. If this is missing, every assertion above passed against
	// somebody else's bus.
	certPath := filepath.Join(dataDir, "bus-tls.crt")
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("stat %q after a successful start: %v; the wrapper must have honoured AGENT_BUS_DATA_DIR", certPath, err)
	}

	// --- status: the same probe, from a second invocation ---
	if code, out := busServe(t, "status"); code != busServeOK {
		t.Fatalf("scripts/bus-serve.sh status = %d, want %d on a running, healthy bus\n%s", code, busServeOK, out)
	}

	// --- the bus is up, so a plaintext request to the SAME port must not be
	// served. This is the live counterpart of TestPlaintextClientIsRejected: the
	// bus under test here was started by the wrapper, not by the harness.
	mustGetHealthz(t, dataDir, listen)
	assertPlaintextRefused(t, listen, nil)

	// --- the container's probe, run as the container runs it ---
	//
	// The BUILT BINARY, not runHealthcheckCommand in-process: Dockerfile's
	// HEALTHCHECK and docker-compose.yml's healthcheck both invoke
	// `agent-bus healthcheck`, so main()'s subcommand dispatch is part of the
	// path under test and an in-process call would skip it.
	bin := filepath.Join(runDir, "bin", "agent-bus")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("stat %q: %v; `bus-serve.sh start` builds the binary there and the container probe is that binary", bin, err)
	}
	probe := exec.Command(bin, "healthcheck", "-data-dir", dataDir, "-addr", listen)
	probeOut, probeErr := probe.CombinedOutput()
	if probeErr != nil {
		t.Fatalf("%s healthcheck -data-dir %s -addr %s failed: %v\n%s\nThis is the EXACT command Dockerfile's HEALTHCHECK runs; a failure here is a container that never reports healthy.",
			bin, dataDir, listen, probeErr, probeOut)
	}
	if !strings.Contains(string(probeOut), "ok https://") {
		t.Errorf("the healthcheck exited 0 but printed %q, want a line naming the https URL it verified", probeOut)
	}

	// --- stop, and the state afterwards ---
	if code, out := busServe(t, "stop"); code != busServeOK {
		t.Fatalf("scripts/bus-serve.sh stop = %d, want %d\n%s", code, busServeOK, out)
	}
	if code, out := busServe(t, "status"); code != busServeNotRunning {
		t.Fatalf("scripts/bus-serve.sh status after stop = %d, want %d (not running)\n%s", code, busServeNotRunning, out)
	}
}
