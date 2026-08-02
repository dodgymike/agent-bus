package main

// Tests for DUR-9: run() must open AND replay the write-ahead log strictly
// after the data-dir lock and strictly before the listener binds, must refuse
// to start (non-zero exit, no listener) when the log cannot be opened, and must
// close the log without damaging it on shutdown.
//
// These run the server in a SUBPROCESS -- os.Args[0] re-executed with
// envRunServer set, which TestMain routes into run() -- for two reasons that a
// same-process test cannot cover: the refusal path has to prove a real non-zero
// PROCESS exit (run() returning an error is only half of the claim; main()
// turning it into os.Exit(1) is the other half), and the happy path has to
// exercise the real signal handler installed by run(), which a test that
// injects into waitAndShutdown deliberately bypasses (see
// TestShutdownReleasesLongPoll in main_test.go).
//
// The harness lives entirely in this file: no production code exists or is
// shaped to support it.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/wal"
)

// Environment contract between the parent test and the re-executed child. All
// four are read ONLY by TestMain below; nothing in main.go knows about them.
const (
	envRunServer = "AGENT_BUS_TEST_RUN_SERVER"
	envDataDir   = "AGENT_BUS_TEST_DATA_DIR"
	envListen    = "AGENT_BUS_TEST_LISTEN"
	envLogLevel  = "AGENT_BUS_TEST_LOG_LEVEL"
)

// Bounds. Every wait in this file is bounded so a regression fails the test
// instead of hanging the suite.
const (
	startupTimeout  = 20 * time.Second
	shutdownTimeout = 15 * time.Second
	pollInterval    = 10 * time.Millisecond
)

// TestMain doubles as the server entry point for the child process. With
// envRunServer set it calls main() itself, so the child is the real startup
// path and not a re-implementation of it.
func TestMain(m *testing.M) {
	if os.Getenv(envRunServer) == "1" {
		os.Exit(runServerChild())
	}
	os.Exit(m.Run())
}

// runServerChild IS main(): it builds argv from the harness environment and
// calls main() directly, so the child exercises parseFlags, Config.validate and
// -- crucially -- main()'s own error-to-exit-code mapping, instead of a copy of
// them that would keep passing while main() drifted. The corrupt-log test's
// "exit 1" and "agent-bus: " prefix are therefore assertions about main.go, not
// about this harness.
//
// main() calls os.Exit on every failure path, so reaching the return can only
// mean a clean run: 0.
func runServerChild() int {
	dataDir := os.Getenv(envDataDir)
	if dataDir == "" {
		// Never silently inherit main.go's ./data default: the tracked data dir
		// is not a test fixture and no test may ever write to it.
		fmt.Fprintf(os.Stderr, "agent-bus: test harness: %s must be set\n", envDataDir)
		return 2
	}
	listen := os.Getenv(envListen)
	if listen == "" {
		// An empty listen address must never become a bind on every interface.
		// The harness only ever wants an ephemeral loopback port.
		listen = "127.0.0.1:0"
	}
	level := os.Getenv(envLogLevel)
	if level == "" {
		level = defaultLogLevel
	}
	os.Args = []string{"agent-bus", "-listen", listen, "-data-dir", dataDir, "-log-level", level}
	main()
	return 0
}

// TestServerOpensWALOnStart is the DUR-9 proof: the name is load-bearing, the
// task's proof_cmd is `go test -race -run TestServerOpensWALOnStart ./...`.
func TestServerOpensWALOnStart(t *testing.T) {
	t.Run("fresh data dir", func(t *testing.T) {
		dir := t.TempDir()
		walPath := filepath.Join(dir, wal.WALFileName)

		proc := startServer(t, dir)
		addr := proc.awaitServerStarted(t)

		// Invariant 6/DUR-9: the log FILE is created at startup, not lazily on
		// the first write. Non-empty because wal.Open writes the 16-byte file
		// header. A regression that drops the wal.Open call entirely leaves no
		// bus.wal here at all.
		fi, err := os.Stat(walPath)
		if err != nil {
			t.Fatalf("stat %q after startup: %v; run() must open the WAL on start", walPath, err)
		}
		if fi.Size() == 0 {
			t.Fatalf("%q is empty after startup, want at least the file header", walPath)
		}

		// THE ORDERING ASSERTION (invariant 5: disk is the truth, memory is only
		// the serving copy). The three lines pin the whole startup sequence as an
		// observable order: the data dir is LOCKED, then the log is opened and
		// replayed inside that lock (wal.Open does not return until replay has
		// finished), and only then does the server announce itself started --
		// which run() logs after srv.Serve is running, so nothing can have been
		// ANSWERED from an unreplayed store.
		//
		// What this does NOT claim: where net.Listen sits. A listener bound early
		// but not yet served answers nothing, so hoisting the bind alone would
		// (correctly) not fail here; "served after replay" is what matters and is
		// what is asserted, reinforced by the mustGetHealthz below.
		lockedIdx := proc.lineIndex(t, msgDirLocked)
		openedIdx := proc.lineIndex(t, msgWALOpened)
		startedIdx := proc.lineIndex(t, msgServerStarted)
		if lockedIdx >= openedIdx || openedIdx >= startedIdx {
			t.Fatalf("stderr line order: %q at %d, %q at %d, %q at %d; want lock < open+replay < serve\n%s",
				msgDirLocked, lockedIdx, msgWALOpened, openedIdx, msgServerStarted, startedIdx, proc.stderr())
		}

		// A fresh log has nothing to replay, and the high-water mark starts at
		// 1: an empty log that reported anything else would mean recovery
		// invented history.
		fields := parseLogfmt(proc.line(t, msgWALOpened))
		wantFresh := map[string]string{
			"data_dir":         dir,
			"path":             walPath,
			"records_replayed": "0",
			"applied":          "0",
			"aborted":          "0",
			"dangling":         "0",
			"next_index":       "1",
			"repaired":         "false",
			"repaired_bytes":   "0",
		}
		for k, want := range wantFresh {
			if got := fields[k]; got != want {
				t.Errorf("%q field %s = %q, want %q (full line: %s)", msgWALOpened, k, got, want, proc.line(t, msgWALOpened))
			}
		}

		// The server really served: proves wal.Open did not merely succeed but
		// also returned in time for the listener to come up.
		mustGetHealthz(t, addr)

		// Shutdown: SIGTERM must be a clean exit 0 (the signal handler run()
		// installs is only reachable in a real process), and Close must leave
		// the log on disk. Truncating or unlinking it on shutdown would be a
		// silent invariant-6 violation that nothing else catches.
		before := mustReadFile(t, walPath)
		proc.signal(t, syscall.SIGTERM)
		if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
			t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
		}

		// CLOSE ACTUALLY RAN, AND RAN BEFORE THE UNLOCK. The size check below
		// cannot see a missing Close: the kernel closes the fd at process exit,
		// so deleting run()'s deferred walLog.Close() leaves bus.wal
		// byte-identical and every other assertion here green. The "write-ahead
		// log closed" line is the only observable proof, and requiring it to
		// precede "data directory lock released" pins the defer-LIFO order
		// main.go treats as load-bearing: flush and close the log while the data
		// dir is still locked, and only then drop the lock.
		//
		// Both lines are written during process teardown, so they are read from
		// the FINAL drained snapshot -- awaitExit returns only after the stderr
		// pipe hit EOF and cmd.Wait() returned, so this cannot race the scanner.
		closedIdx := proc.lineIndex(t, msgWALClosed)
		releasedIdx := proc.lineIndex(t, msgLockReleased)
		if closedIdx >= releasedIdx {
			t.Fatalf("stderr line order on shutdown: %q at %d, %q at %d; the log must be closed while the data dir is still locked\n%s",
				msgWALClosed, closedIdx, msgLockReleased, releasedIdx, proc.stderr())
		}

		after := mustReadFile(t, walPath)
		if len(after) < len(before) {
			t.Fatalf("%q shrank across shutdown: %d bytes -> %d bytes; Close must never truncate the log",
				walPath, len(before), len(after))
		}
	})

	t.Run("replays an existing log", func(t *testing.T) {
		// REPLAY IS REAL. Without this subtest the suite only proves a file was
		// created; here the data dir is seeded with committed transactions
		// BEFORE the server starts, and the startup line has to account for
		// every one of them. A run() that opened the log with replay disabled,
		// or that created a second empty log somewhere else, fails here.
		dir := t.TempDir()
		walPath := filepath.Join(dir, wal.WALFileName)

		const seeded = 3
		seed, err := wal.Open(wal.LogOptions{Dir: dir})
		if err != nil {
			t.Fatalf("seeding wal.Open(%q): %v", dir, err)
		}
		var last wal.Committed
		for i := 0; i < seeded; i++ {
			body := json.RawMessage(fmt.Sprintf(`{"n":%d}`, i+1))
			last, err = seed.Write(wal.Entry{Kind: "test", Body: body})
			if err != nil {
				t.Fatalf("seeding write %d: %v", i, err)
			}
		}
		if err := seed.Close(); err != nil {
			t.Fatalf("closing the seeded log: %v", err)
		}

		// Expectations are DERIVED from what was actually written, not guessed:
		// the two-phase write path emits one PREPARE and one COMMIT per Write
		// (2*seeded records), each Write reached commit (applied == seeded),
		// and the next append must land strictly above the last index handed
		// out, so next_index == last commit index + 1.
		wantRecords := 2 * seeded
		wantNext := last.CommitIndex + 1

		proc := startServer(t, dir)
		addr := proc.awaitServerStarted(t)

		openedLine := proc.line(t, msgWALOpened)
		fields := parseLogfmt(openedLine)
		checks := []struct {
			key  string
			want string
			why  string
		}{
			{"path", walPath, "the server must replay the SEEDED log, not a fresh one elsewhere"},
			{"records_replayed", strconv.Itoa(wantRecords), "one prepare + one commit per seeded write must be read back"},
			{"applied", strconv.Itoa(seeded), "every seeded transaction reached commit and must be recovered"},
			{"aborted", "0", "nothing was aborted"},
			{"dangling", "0", "every prepare was committed before the seeding log was closed"},
			{"next_index", strconv.FormatUint(wantNext, 10), "the high-water mark must sit above every index ever written"},
			{"repaired", "false", "an intact log must never be truncated by recovery (invariant 6)"},
			{"repaired_bytes", "0", "an intact log must never lose a byte"},
		}
		for _, c := range checks {
			if got := fields[c.key]; got != c.want {
				t.Errorf("%q field %s = %q, want %q: %s\nline: %s", msgWALOpened, c.key, got, c.want, c.why, openedLine)
			}
		}

		// Ordering holds on the replay path too -- this is the case that
		// actually matters, since here there IS history to lose.
		if openedIdx, startedIdx := proc.lineIndex(t, msgWALOpened), proc.lineIndex(t, msgServerStarted); openedIdx >= startedIdx {
			t.Fatalf("replay finished at line %d but the server started at line %d; nothing may be served before replay completes\n%s",
				openedIdx, startedIdx, proc.stderr())
		}

		mustGetHealthz(t, addr)

		beforeSize := mustFileSize(t, walPath)
		proc.signal(t, syscall.SIGTERM)
		if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
			t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
		}
		// Close ran, and ran before the unlock, on the path that has recovered
		// history to lose too (see the fresh-dir subtest for why the size check
		// alone cannot see a deleted Close).
		if closedIdx, releasedIdx := proc.lineIndex(t, msgWALClosed), proc.lineIndex(t, msgLockReleased); closedIdx >= releasedIdx {
			t.Fatalf("stderr line order on shutdown: %q at %d, %q at %d; the replayed log must be closed while the data dir is still locked\n%s",
				msgWALClosed, closedIdx, msgLockReleased, releasedIdx, proc.stderr())
		}
		if afterSize := mustFileSize(t, walPath); afterSize < beforeSize {
			t.Fatalf("seeded log shrank across shutdown: %d -> %d bytes; recovered history must survive a clean stop",
				beforeSize, afterSize)
		}
	})
}

// TestServerOpensWALOnStartRefusesACorruptLog covers the fatal path. The name
// deliberately shares the TestServerOpensWALOnStart prefix so the proof's
// unanchored -run regex covers it too.
//
// The seeded damage is a garbage FILE HEADER, which recovery must refuse rather
// than treat as a truncatable torn tail: a torn tail is bytes whose write never
// completed, but a bad header is damage to bytes that were fully written, and
// guessing there would risk discarding acknowledged history (invariant 6).
func TestServerOpensWALOnStartRefusesACorruptLog(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, wal.WALFileName)

	corrupt := []byte(strings.Repeat("X", 64))
	if err := os.WriteFile(walPath, corrupt, 0o600); err != nil {
		t.Fatalf("writing the corrupt log %q: %v", walPath, err)
	}
	before := mustReadFile(t, walPath)

	proc := startServer(t, dir)

	// Exit code 1 exactly: main() maps any run() error to os.Exit(1), and a
	// regression that logs the failure and carries on serving would exit 0 (or
	// not exit at all, tripping the timeout below).
	code := proc.awaitExit(t, startupTimeout)
	if code != 1 {
		t.Fatalf("exit code with a corrupt %q = %d, want 1\n%s", wal.WALFileName, code, proc.stderr())
	}

	stderr := proc.stderr()
	const wantPrefix = "agent-bus: opening the write-ahead log in"
	if !strings.Contains(stderr, wantPrefix) {
		t.Errorf("stderr does not contain %q; the operator must be told WHICH stage refused\n%s", wantPrefix, stderr)
	}
	if !strings.Contains(stderr, dir) {
		t.Errorf("stderr does not name the data dir %q; a refusal that hides the path is not actionable\n%s", dir, stderr)
	}

	// Nothing was served. Both checks matter: no "server started" line, and no
	// addr= field anywhere, so a regression cannot satisfy the first by simply
	// renaming the message.
	if strings.Contains(stderr, msgServerStarted) {
		t.Errorf("stderr contains %q after a failed WAL open; the listener must never bind on this path\n%s", msgServerStarted, stderr)
	}
	if strings.Contains(stderr, " addr=") {
		t.Errorf("stderr reports a listen address after a failed WAL open; nothing may listen\n%s", stderr)
	}

	// A REFUSAL MUST NOT REPAIR. The corrupt bytes are still there, unchanged
	// and un-truncated, so an operator can take a copy and diagnose it; a
	// recovery pass that "fixed" a bad header by cutting the file would destroy
	// the evidence and, on a real log, the history behind it.
	after := mustReadFile(t, walPath)
	if len(after) != len(before) {
		t.Fatalf("corrupt %q changed size across the refused start: %d -> %d bytes; a refusal must never truncate",
			wal.WALFileName, len(before), len(after))
	}
	if string(after) != string(before) {
		t.Fatalf("corrupt %q changed contents across the refused start; a refusal must never rewrite the log", wal.WALFileName)
	}
}

// Log messages the assertions above key on. They are the operator-visible
// contract of run(); renaming one in main.go must fail these tests.
const (
	msgDirLocked     = `msg="data directory locked"`
	msgWALOpened     = `msg="write-ahead log opened"`
	msgServerStarted = `msg="server started"`
	msgWALClosed     = `msg="write-ahead log closed"`
	msgLockReleased  = `msg="data directory lock released"`
)

// serverProc is a running (or finished) child server: its captured stderr, and
// a bounded way to wait for it.
type serverProc struct {
	cmd *exec.Cmd

	mu    sync.Mutex
	lines []string

	done    chan struct{} // closed once the process has exited AND stderr is drained
	waitErr error         // the Wait() result; read only after done is closed
}

// startServer re-executes the test binary as a server bound to 127.0.0.1:0 (an
// ephemeral port: never a fixed one, and never the tracked ./data dir) against
// dataDir, and registers a cleanup that kills it however the test ends.
func startServer(t *testing.T, dataDir string) *serverProc {
	t.Helper()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		envRunServer+"=1",
		envDataDir+"="+dataDir,
		envListen+"=127.0.0.1:0",
		envLogLevel+"=debug",
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child server %q: %v", os.Args[0], err)
	}

	p := &serverProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			p.mu.Lock()
			p.lines = append(p.lines, sc.Text())
			p.mu.Unlock()
		}
		p.waitErr = cmd.Wait()
	}()

	t.Cleanup(func() {
		select {
		case <-p.done:
			return
		default:
		}
		_ = cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(shutdownTimeout):
			t.Errorf("child server %d did not exit after Kill", cmd.Process.Pid)
		}
	})
	return p
}

// snapshot returns a copy of the stderr lines seen so far.
func (p *serverProc) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.lines))
	copy(out, p.lines)
	return out
}

// stderr renders everything captured so far, for failure messages.
func (p *serverProc) stderr() string {
	return "--- child stderr ---\n" + strings.Join(p.snapshot(), "\n") + "\n--- end ---"
}

// awaitLine blocks until a stderr line contains want, the process exits, or the
// bound expires.
func (p *serverProc) awaitLine(t *testing.T, want string, bound time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		for _, l := range p.snapshot() {
			if strings.Contains(l, want) {
				return l
			}
		}
		select {
		case <-p.done:
			// One last look: the line may have arrived with the final flush.
			for _, l := range p.snapshot() {
				if strings.Contains(l, want) {
					return l
				}
			}
			t.Fatalf("child server exited (%v) before logging %q\n%s", p.waitErr, want, p.stderr())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %q on the child's stderr\n%s", bound, want, p.stderr())
		}
		time.Sleep(pollInterval)
	}
}

// awaitServerStarted waits for the startup line and returns the address it
// reports, so the test talks to the port the kernel actually assigned.
func (p *serverProc) awaitServerStarted(t *testing.T) string {
	t.Helper()
	line := p.awaitLine(t, msgServerStarted, startupTimeout)
	addr := parseLogfmt(line)["addr"]
	if addr == "" {
		t.Fatalf("%q line has no addr= field: %s", msgServerStarted, line)
	}
	return addr
}

// line returns the (already captured) stderr line containing want.
func (p *serverProc) line(t *testing.T, want string) string {
	t.Helper()
	for _, l := range p.snapshot() {
		if strings.Contains(l, want) {
			return l
		}
	}
	t.Fatalf("no stderr line contains %q\n%s", want, p.stderr())
	return ""
}

// lineIndex returns the position of the first stderr line containing want.
func (p *serverProc) lineIndex(t *testing.T, want string) int {
	t.Helper()
	for i, l := range p.snapshot() {
		if strings.Contains(l, want) {
			return i
		}
	}
	t.Fatalf("no stderr line contains %q\n%s", want, p.stderr())
	return -1
}

// signal delivers sig to the child.
func (p *serverProc) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signalling the child with %v: %v", sig, err)
	}
}

// awaitExit waits for the child to exit within bound and returns its exit code.
func (p *serverProc) awaitExit(t *testing.T, bound time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(bound):
		t.Fatalf("child server did not exit within %s\n%s", bound, p.stderr())
	}
	if p.waitErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(p.waitErr, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("waiting for the child server: %v\n%s", p.waitErr, p.stderr())
	return -1
}

// mustGetHealthz asserts GET /healthz answers 200 at addr, i.e. the process is
// really serving and not merely alive.
func mustGetHealthz(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET http://%s/healthz: %v", addr, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %q: %v", path, err)
	}
	return b
}

func mustFileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return fi.Size()
}

// parseLogfmt splits one logfmt record ("k=v k=\"quoted v\"") into its fields.
// internal/logging quotes a value whenever it is not bare-printable, so quoted
// values are unquoted here with strconv.Unquote, the same function that wrote
// them.
func parseLogfmt(line string) map[string]string {
	fields := make(map[string]string)
	for i := 0; i < len(line); {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			break
		}
		key := line[i : i+eq]
		i += eq + 1
		if i < len(line) && line[i] == '"' {
			j := i + 1
			for j < len(line) {
				if line[j] == '\\' {
					j += 2
					continue
				}
				if line[j] == '"' {
					j++
					break
				}
				j++
			}
			raw := line[i:j]
			if v, err := strconv.Unquote(raw); err == nil {
				fields[key] = v
			} else {
				fields[key] = raw
			}
			i = j
			continue
		}
		if sp := strings.IndexByte(line[i:], ' '); sp >= 0 {
			fields[key] = line[i : i+sp]
			i += sp
		} else {
			fields[key] = line[i:]
			i = len(line)
		}
	}
	return fields
}
