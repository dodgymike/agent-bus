package main

// AUTH-10-WIRING: the two behavioural proofs that the operator principal is
// WIRED INTO THE SERVER, not merely implemented beside it.
//
// AUTH-10 landed internal/auth's OperatorRegistry and cmd/agent-bus/operator.go
// with an exhaustive unit suite, all of it green, while the server did neither
// of the things an operator would call the feature. That is the shape these
// tests exist to catch, and it is the same shape MSG-FU-SUFFIXFLOOR had: a
// package that is correct and a composition root that never constructs it. No
// unit test in internal/auth can tell those two states apart, so both tests
// below start a REAL server process (or exec the REAL compiled binary) rather
// than assembling the pieces themselves.
//
//   - TestOperatorRecordsSurviveServerRestart is the headline. Operator records
//     were passed over at server replay in COMPLETE SILENCE, because
//     auth.MultiplexApplier returns nil for a kind it does not own and main.go's
//     applier map did not own "operator". That is the silent discard invariant 6
//     rates as the defect, and it is fail-OPEN for a revocation: the record that
//     takes an operator's authority away was the one being dropped.
//
//   - TestOperatorSubcommandIsReachableFromArgv is invariant 7's half. The
//     subcommand existed and was untestable from a shell: main() dispatched
//     invite/healthcheck/peer/key/log on os.Args[1] and not operator, so
//     `agent-bus operator …` fell through to parseFlags and was refused as an
//     unexpected argument.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// msgOperatorsRecovered is the startup line run() emits for the operator plane.
// Like the four in wal_startup_test.go it is OPERATOR-VISIBLE CONTRACT: renaming
// it in main.go must fail this test rather than quietly remove the only signal
// that operator records were replayed at all.
const msgOperatorsRecovered = `msg="operator registry recovered from the append-only log`

// TestOperatorRecordsSurviveServerRestart is the acceptance bar for
// AUTH-10-WIRING: a server started over a data directory holding operator
// records must REPLAY them, and a REVOKED operator must still be revoked
// afterwards.
//
// # Why the two counts, and why this cannot pass on the log line alone
//
// The assertion is not "the line is present". It is that operators_recovered is
// 2 and live_operators is 1. Both numbers come from the registry's own maps, so
// they are non-zero only if auth.OperatorRecordKind is in main.go's applier map
// and wal.Open fed it every record; adding the log line WITHOUT the registration
// prints 0 and 0 and fails here. And the two numbers fail for different reasons,
// which is the point of asserting both:
//
//   - operators_recovered == 2 fails if the two `add` records were skipped.
//   - live_operators == 1 fails if the `revoke` record was skipped -- the
//     FAIL-OPEN direction, where a bus restarts believing a revoked operator is
//     still a principal.
//
// A test that checked only the total would go green over a dropped revocation,
// which is the one discard that actually grants authority.
func TestOperatorRecordsSurviveServerRestart(t *testing.T) {
	dir, busID := operatorDataDir(t)

	// Three REAL records through the REAL offline subcommand -- the same code
	// path an operator's shell takes, under the same directory lock. Nothing
	// here hand-writes a record, so a change to the on-disk operator record
	// cannot leave this test asserting against a format the CLI no longer emits.
	aPub, aFP := operatorCredential(t, 0x21)
	bPub, bFP := operatorCredential(t, 0x22)

	code, stdout, stderr := runOperator(t, "add", "-data-dir", dir,
		"-name", "keeper", "-auth-pub", aPub, "-cert-fingerprint", aFP, "-json")
	if code != exitOperatorOK {
		t.Fatalf("operator add keeper exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	keeperID := onlyOperatorID(t, stdout)

	code, stdout, stderr = runOperator(t, "add", "-data-dir", dir,
		"-name", "leaver", "-auth-pub", bPub, "-cert-fingerprint", bFP, "-json")
	if code != exitOperatorOK {
		t.Fatalf("operator add leaver exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	leaverID := onlyOperatorID(t, stdout)

	code, stdout, stderr = runOperator(t, "revoke", "-data-dir", dir,
		"-id", leaverID, "-reason", "laptop lost", "-json")
	if code != exitOperatorOK {
		t.Fatalf("operator revoke exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// The floors file a steady-state restart would already have. Seeded rather
	// than disarmed with -backfill-suffix-floors, for the reason
	// seedSuffixFloorsFile documents: the flag would take the AUTH-3-FU-FAILOPEN
	// guard out of the path this test claims to exercise. Legal here because
	// this dir's log holds operator records only -- no agent id has ever been
	// minted on it.
	seedSuffixFloorsFile(t, dir)

	// THE RESTART. A real child process running the real startup path.
	proc := startServer(t, dir)
	proc.awaitServerStarted(t)

	line := proc.line(t, msgOperatorsRecovered)
	fields := parseLogfmt(line)
	if got := fields["bus_id"]; got != busID {
		t.Fatalf("the operator recovery line names bus_id %q, want %q\n%s", got, busID, line)
	}
	if got := atoiField(t, fields, "operators_recovered", line); got != 2 {
		t.Fatalf("operators_recovered = %d, want 2 (%q and %q were written to this log before the start). "+
			"0 means main.go's applier map does not own auth.OperatorRecordKind and every operator record was "+
			"passed over in silence at replay -- the invariant 6 defect AUTH-10-WIRING exists to close.\n%s",
			got, keeperID, leaverID, line)
	}
	if got := atoiField(t, fields, "live_operators", line); got != 1 {
		t.Fatalf("live_operators = %d, want 1 (%q is live, %q was revoked before this start). "+
			"2 means the REVOCATION record did not replay, which is the FAIL-OPEN direction: this bus came back "+
			"up treating a revoked operator as a principal.\n%s", got, keeperID, leaverID, line)
	}

	// The recovery line must come BEFORE the server serves. An operator plane
	// rebuilt after the socket answers would leave a window in which the bus is
	// live and knows about no revocation -- the same ordering wal_startup_test
	// pins for the log itself.
	if recIdx, startedIdx := proc.lineIndex(t, msgOperatorsRecovered), proc.lineIndex(t, msgServerStarted); recIdx >= startedIdx {
		t.Fatalf("the operator registry was reported recovered at line %d, AFTER %s at line %d; replay must finish before anything serves\n%s",
			recIdx, msgServerStarted, startedIdx, proc.stderr())
	}

	proc.signal(t, syscall.SIGTERM)
	if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
	}

	// And the offline view agrees with the server's, which is what makes the
	// numbers above a claim about STATE rather than about one log line: the same
	// log, replayed by the subcommand, still reports exactly one live operator.
	code, stdout, stderr = runOperator(t, "list", "-data-dir", dir, "-json")
	if code != exitOperatorOK {
		t.Fatalf("operator list exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	listed := decodeOperatorJSON(t, stdout)
	live, _ := listed["operators"].([]interface{})
	if len(live) != 1 {
		t.Fatalf("operator list reports %d live operators after the restart, want 1: %s", len(live), stdout)
	}
	if rec, _ := live[0].(map[string]interface{}); rec["operator_id"] != keeperID {
		t.Fatalf("the surviving live operator is %v, want %q", rec["operator_id"], keeperID)
	}
}

// TestOperatorSubcommandIsReachableFromArgv proves gap 1 of AUTH-10-WIRING
// against the COMPILED BINARY, which is the only thing that can prove it.
//
// runOperatorCommand is already covered exhaustively by operator_test.go, and
// every one of those tests passed while `agent-bus operator list` was refused at
// the command line: they call the function directly and therefore never touch
// main()'s argv dispatch. So this test builds the binary and runs it the way an
// operator's shell does. Invariant 7 -- a capability with no reachable
// subcommand is the missing half of the task, not a shipped feature.
func TestOperatorSubcommandIsReachableFromArgv(t *testing.T) {
	bin := buildAgentBusBinary(t)
	dir, busID := operatorDataDir(t)
	pub, fp := operatorCredential(t, 0x31)

	// add, through argv.
	stdout, stderr, code := runBinary(t, bin, "operator", "add", "-data-dir", dir,
		"-name", "argv", "-auth-pub", pub, "-cert-fingerprint", fp, "-json")
	if code != exitOperatorOK {
		t.Fatalf("`agent-bus operator add` exit = %d, want 0. A non-zero exit with %q on stderr means main() never "+
			"dispatched os.Args[1] == %q and the argument fell through to parseFlags.\nstdout: %s\nstderr: %s",
			code, "unexpected argument", operatorCommandName, stdout, stderr)
	}
	addedID := onlyOperatorID(t, stdout)

	// list, through argv: the subcommand's own view of what add just wrote.
	stdout, stderr, code = runBinary(t, bin, "operator", "list", "-data-dir", dir, "-json")
	if code != exitOperatorOK {
		t.Fatalf("`agent-bus operator list` exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	listed := decodeOperatorJSON(t, stdout)
	if listed["bus_id"] != busID {
		t.Fatalf("`operator list` bus_id = %v, want %q", listed["bus_id"], busID)
	}
	if ops, _ := listed["operators"].([]interface{}); len(ops) != 1 {
		t.Fatalf("`operator list` reports %d operators, want 1: %s", len(ops), stdout)
	}

	// revoke, through argv. THIS is the one that had no reachable caller at all:
	// it is the only mechanism in the design for taking an operator's authority
	// away, so a subcommand that cannot be invoked is a bus whose operator
	// credentials cannot be withdrawn.
	stdout, stderr, code = runBinary(t, bin, "operator", "revoke", "-data-dir", dir,
		"-id", addedID, "-reason", "argv reachability", "-json")
	if code != exitOperatorOK {
		t.Fatalf("`agent-bus operator revoke` exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	stdout, _, code = runBinary(t, bin, "operator", "list", "-data-dir", dir, "-json")
	if code != exitOperatorOK {
		t.Fatalf("`agent-bus operator list` after revoke exit = %d, want 0: %s", code, stdout)
	}
	listed = decodeOperatorJSON(t, stdout)
	if ops, _ := listed["operators"].([]interface{}); len(ops) != 0 {
		t.Fatalf("`operator list` still reports %d live operators after revoke, want 0: %s", len(ops), stdout)
	}

	// The subcommand is also ANNOUNCED. An operator who cannot find `operator`
	// in -h has no way to learn that revocation exists, and -h is the whole
	// discovery surface this binary has.
	//
	// PINNED TO THE SUBCOMMAND LINE ITSELF, not to the word "operator", and the
	// difference is the whole value of this assertion. Searching -h for
	// operatorCommandName PASSES ON AN UNRELATED MATCH: `-backfill-suffix-floors`
	// describes itself as a "one-time operator opt-in", so a bare Contains check
	// stays green on a build where the Subcommands entry was never added at all.
	// Verified against a binary compiled from HEAD before this task -- it prints
	// that line and no operator subcommand. Both strings below appear ONLY in the
	// entry parseFlags' fs.Usage emits.
	stdout, stderr, code = runBinary(t, bin, "-h")
	help := stdout + stderr
	for _, want := range []string{
		" " + operatorCommandName + " keygen|add|list|revoke",
		"manage OPERATOR principals",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("`agent-bus -h` does not announce the %q subcommand: no %q in its usage (exit %d).\n%s",
				operatorCommandName, want, code, help)
		}
	}
}

// onlyOperatorID pulls the single operator id out of an `operator add|revoke`
// --json result and fails if the shape is not the expected one-element array.
func onlyOperatorID(t *testing.T, stdout string) string {
	t.Helper()
	obj := decodeOperatorJSON(t, stdout)
	ops, _ := obj["operators"].([]interface{})
	if len(ops) != 1 {
		t.Fatalf("expected exactly one operator in the result, got %d: %s", len(ops), stdout)
	}
	rec, _ := ops[0].(map[string]interface{})
	id, _ := rec["operator_id"].(string)
	if id == "" {
		t.Fatalf("the result carries no operator_id: %s", stdout)
	}
	return id
}

// atoiField reads a numeric logfmt field, failing with the whole line when it is
// absent -- a missing field and a zero one are different bugs and must not both
// arrive as 0.
func atoiField(t *testing.T, fields map[string]string, name, line string) int {
	t.Helper()
	raw, ok := fields[name]
	if !ok {
		t.Fatalf("the operator recovery line carries no %s= field:\n%s", name, line)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a number: %v\n%s", name, raw, err, line)
	}
	return n
}

// buildAgentBusBinary compiles THIS package into a temp directory and returns
// the path.
//
// The test binary cannot stand in for it: its own TestMain owns argv, so
// exec'ing os.Args[0] with "operator" would be answered by the testing package's
// flag parser and would prove nothing about main(). Compiling is the only way to
// exercise the dispatch an operator's shell hits.
func buildAgentBusBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agent-bus")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building the agent-bus binary: %v\n%s", err, out)
	}
	return bin
}

// runBinary runs the compiled binary with args and returns stdout, stderr and
// the exit code, bounded so a subcommand that waits on something cannot hang the
// suite.
func runBinary(t *testing.T, bin string, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr strings.Builder
	cmd := exec.Command(bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// No TTY and no inherited AGENT_BUS_* harness variables: an agent shelling
	// out gets neither, and envRunServer in particular would turn a subcommand
	// invocation into a server start.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s %s: %v", bin, strings.Join(args, " "), err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			var ee *exec.ExitError
			if !asExitError(err, &ee) {
				t.Fatalf("running %s %s: %v\nstderr: %s", bin, strings.Join(args, " "), err, stderr.String())
			}
			return stdout.String(), stderr.String(), ee.ExitCode()
		}
		return stdout.String(), stderr.String(), 0
	case <-time.After(shutdownTimeout):
		_ = cmd.Process.Kill()
		t.Fatalf("%s %s did not exit within %s\nstdout: %s\nstderr: %s",
			bin, strings.Join(args, " "), shutdownTimeout, stdout.String(), stderr.String())
	}
	return "", "", 0
}

// asExitError is errors.As specialised, kept local so runBinary reads as one
// thing.
func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
